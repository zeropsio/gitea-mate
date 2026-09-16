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

// Config builds a Client.
type Config struct {
	// BaseURL is GITEA_URL — the instance's origin, no /api/v1.
	BaseURL string
	// AdminToken is the site admin's API token.
	AdminToken string
	// AdminUser and AdminPassword are the site admin's basic-auth credentials.
	// They are needed only by the token routes, which refuse an API token.
	AdminUser     string
	AdminPassword string
	HTTP          *http.Client
}

// Client talks to one Gitea.
type Client struct {
	base          string
	token         string
	adminUser     string
	adminPassword string
	http          *http.Client
}

// New builds an admin client.
func New(cfg Config) *Client {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		base:          strings.TrimSuffix(cfg.BaseURL, "/"),
		token:         cfg.AdminToken,
		adminUser:     cfg.AdminUser,
		adminPassword: cfg.AdminPassword,
		http:          hc,
	}
}

// AsToken returns a client that acts as an arbitrary Gitea token and holds no
// admin credential at all. It is how /mate/repository resolves a Mate's bot and
// how /deploy will resolve a job.
func (c *Client) AsToken(token string) *Client {
	return &Client{base: c.base, token: token, http: c.http}
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

func (c *Client) doStatus(ctx context.Context, method, path string, in, out any, a auth) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("gitea: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPath+path, body)
	if err != nil {
		return 0, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	switch a {
	case authBasic:
		if c.adminUser == "" || c.adminPassword == "" {
			return 0, errors.New("gitea: this call needs the site admin's basic-auth credentials, which are not configured")
		}
		req.SetBasicAuth(c.adminUser, c.adminPassword)
	default:
		req.Header.Set("Authorization", "token "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, &APIError{Status: resp.StatusCode, Message: msg.Message, Path: method + " " + path}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("gitea: %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
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
