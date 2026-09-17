// Package gitea is the slice of Gitea's API the broker drives.
//
// Three credentials, deliberately distinct:
//
//   - the site-admin API token, for almost everything;
//   - the site admin's username and password, for the token routes alone —
//     /users/{login}/tokens refuses an API token with 401 "auth required"
//     (measured on Gitea 1.27.2), so minting a bot's credential needs basic
//     auth;
//   - an arbitrary caller's token, for the two reads that answer "who is
//     this?" — GET /user and GET /repos/{o}/{r}/actions/jobs/{id}.
//
// The first two are the site admin's pair, and the client never holds them:
// it asks an [AdminSource] for each call, and asks again when Gitea refuses
// what it was given. That is how the broker outlives a pair that had not
// reached its container when it started, or that Gitea's first boot minted
// anew (internal/siteadmin).
//
// Nothing here ever logs or returns a token value except the one call whose
// whole purpose is to mint one.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiPath is Gitea's REST prefix.
const apiPath = "/api/v1"

// DefaultTimeout bounds one call.
const DefaultTimeout = 30 * time.Second

// pageSize is what every list call asks for. Gitea caps it at MAX_RESPONSE_ITEMS
// (50 by default), so the paging loop below is what actually reads a long list.
const pageSize = 50

// AdminCredentials is the site admin's pair: the API token almost every call
// carries, and the password the token routes take instead.
type AdminCredentials struct {
	Token    string
	Password string
}

// AdminSource hands the client the site admin's credentials. The client asks
// for each call, and when Gitea answers 401 to the pair it was given it asks
// again — Refused — and retries that call once with the answer. A source that
// answers the refused pair back has nothing new to try, and the 401 stands.
type AdminSource interface {
	// Admin is the pair a call should carry. An error is the call's error:
	// Gitea is never asked with nothing.
	Admin(ctx context.Context) (AdminCredentials, error)
	// Refused says Gitea answered 401 to used, and answers the pair to retry
	// with.
	Refused(ctx context.Context, used AdminCredentials) (AdminCredentials, error)
}

// StaticAdmin is an AdminSource that always answers one pair — a test, the
// lab, a local run. A refusal is final: there is nothing else to answer.
type StaticAdmin AdminCredentials

// Admin implements AdminSource.
func (s StaticAdmin) Admin(context.Context) (AdminCredentials, error) {
	return AdminCredentials(s), nil
}

// Refused implements AdminSource: the same pair, which the client knows not
// to retry with.
func (s StaticAdmin) Refused(context.Context, AdminCredentials) (AdminCredentials, error) {
	return AdminCredentials(s), nil
}

// Config builds a Client.
type Config struct {
	// BaseURL is GITEA_URL — the instance's origin, no /api/v1.
	BaseURL string
	// Admin is where the site admin's pair comes from. Nil means the static
	// pair below.
	Admin AdminSource
	// AdminToken and AdminPassword are the static pair, read only when Admin
	// is nil.
	AdminToken    string
	AdminPassword string
	// AdminUser is the site admin's login, which basic auth carries beside
	// the password.
	AdminUser string
	HTTP      *http.Client
}

// Client talks to one Gitea.
type Client struct {
	base      string
	admin     AdminSource
	adminUser string
	http      *http.Client
}

// New builds an admin client.
func New(cfg Config) *Client {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	admin := cfg.Admin
	if admin == nil {
		admin = StaticAdmin{Token: cfg.AdminToken, Password: cfg.AdminPassword}
	}
	return &Client{
		base:      strings.TrimSuffix(cfg.BaseURL, "/"),
		admin:     admin,
		adminUser: cfg.AdminUser,
		http:      hc,
	}
}

// AsToken returns a client that acts as an arbitrary Gitea token and holds no
// admin credential at all. It is how /mate/repository resolves a Mate's bot and
// how /deploy will resolve a job.
func (c *Client) AsToken(token string) *Client {
	return &Client{base: c.base, admin: StaticAdmin{Token: token}, http: c.http}
}

// BaseURL is the instance origin this client talks to.
func (c *Client) BaseURL() string { return c.base }

// APIError is a refusal from Gitea.
type APIError struct {
	Status  int
	Message string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitea: %d on %s: %s", e.Status, e.Path, e.Message)
}

// Status returns err's HTTP status if it is an APIError, else 0.
func Status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// IsNotFound reports whether err is Gitea's 404.
func IsNotFound(err error) bool { return Status(err) == http.StatusNotFound }

// IsConflict reports whether err is Gitea's 409 — "it already exists".
func IsConflict(err error) bool { return Status(err) == http.StatusConflict }

// IsMethodNotAllowed is Gitea's answer to a merge it cannot do as the request
// stands: a conflict, a check still running, or an empty request.
func IsMethodNotAllowed(err error) bool { return Status(err) == http.StatusMethodNotAllowed }

// auth says which credential a call uses.
type auth int

const (
	authToken auth = iota // Authorization: token <api token>
	authBasic             // the site admin's username and password
)

func (c *Client) do(ctx context.Context, method, path string, in, out any, a auth) error {
	_, err := c.doStatus(ctx, method, path, in, out, a)
	return err
}

// maxJSON bounds one JSON answer.
const maxJSON = 16 << 20

func (c *Client) doStatus(ctx context.Context, method, path string, in, out any, a auth) (int, error) {
	var body []byte
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("gitea: encode %s %s: %w", method, path, err)
		}
		body = raw
	}
	status, raw, err := c.send(ctx, method, path, body, a, maxJSON)
	if err != nil {
		return status, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("gitea: %s %s: decode: %w", method, path, err)
		}
	}
	return status, nil
}

// send makes one call with the pair the source answers, and once more with a
// fresh pair when Gitea refuses the first. It answers the status and the body
// of a 2xx, or the refusal as an APIError.
func (c *Client) send(ctx context.Context, method, path string, body []byte, a auth, limit int64) (int, []byte, error) {
	creds, err := c.admin.Admin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("gitea: %s %s: the site admin's credentials: %w", method, path, err)
	}
	for attempt := 0; ; attempt++ {
		status, raw, err := c.once(ctx, method, path, body, a, creds, limit)
		if status != http.StatusUnauthorized || attempt > 0 {
			return status, raw, err
		}
		fresh, ferr := c.admin.Refused(ctx, creds)
		if ferr != nil {
			return status, raw, fmt.Errorf("%w; and the site admin's credentials could not be read again: %w", err, ferr)
		}
		if fresh == creds {
			return status, raw, err
		}
		creds = fresh
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, a auth, creds AdminCredentials, limit int64) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPath+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	switch a {
	case authBasic:
		if c.adminUser == "" || creds.Password == "" {
			return 0, nil, errors.New("gitea: this call needs the site admin's basic-auth credentials, which are not configured")
		}
		req.SetBasicAuth(c.adminUser, creds.Password)
	default:
		req.Header.Set("Authorization", "token "+creds.Token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, nil, &APIError{Status: resp.StatusCode, Message: msg.Message, Path: method + " " + path}
	}
	return resp.StatusCode, raw, nil
}

// exists turns a call whose 404 means "no" into a boolean.
func (c *Client) exists(ctx context.Context, path string) (bool, error) {
	err := c.do(ctx, http.MethodGet, path, nil, nil, authToken)
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// paged reads every page of a list endpoint. fetch appends one page's rows and
// returns how many it read.
func paged(fetch func(page int) (int, error)) error {
	for page := 1; ; page++ {
		n, err := fetch(page)
		if err != nil {
			return err
		}
		if n < pageSize {
			return nil
		}
	}
}

func withPage(path string, page int) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "page=" + strconv.Itoa(page) + "&limit=" + strconv.Itoa(pageSize)
}

func esc(s string) string { return url.PathEscape(s) }
