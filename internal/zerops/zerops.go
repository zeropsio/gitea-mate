// Package zerops is the slice of the Zerops REST API the broker needs, over
// plain net/http.
//
// Every request/response shape here was measured, not read: the fork's ledger
// sections of 2026-09-15 and 2026-09-16 (docs/internals/zerops/verified.md).
// Two rules hold everywhere:
//
//   - every response's Date header is captured, because the throwaway check
//     compares a token's `created` against the API's clock and never the
//     container's;
//   - a list that comes back short of its declared total is reported as such,
//     because the rights loop must never mistake a truncated page for a
//     shrunken org.
package zerops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// basePath is the platform's public REST prefix.
const basePath = "/api/rest/public"

// DefaultTimeout bounds one API call. A pass of the rights loop makes many.
const DefaultTimeout = 30 * time.Second

// Client talks to one Zerops API as one token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New builds a client. apiURL is ZEROPS_API_URL; hc may be nil.
func New(apiURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: strings.TrimSuffix(apiURL, "/"), token: token, http: hc}
}

// APIError is a refusal from the platform. Its Code is the platform's own
// (insufficientPermissions, notAuthorized, roleLevelExceeded …), which callers
// match on rather than on a message.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("zerops api: %d %s: %s", e.Status, e.Code, e.Message)
}

// Status returns the HTTP status of err if it is an APIError, else 0.
func Status(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// Code returns the platform error code of err if it is an APIError, else "".
func Code(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// do makes one call and decodes out, if out is not nil. It returns the Date
// header of the response, which is the API's clock.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (time.Time, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return time.Time{}, fmt.Errorf("zerops api: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+basePath+path, body)
	if err != nil {
		return time.Time{}, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	date := parseDate(resp.Header.Get("Date"))
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return date, fmt.Errorf("zerops api: %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		// The body is the platform's {code, message}. A gateway's HTML is not,
		// and the status alone then has to speak.
		_ = json.Unmarshal(raw, apiErr)
		if apiErr.Code == "" {
			apiErr.Code = "http_" + fmt.Sprint(resp.StatusCode)
		}
		return date, apiErr
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return date, fmt.Errorf("zerops api: %s %s: decode: %w", method, path, err)
		}
	}
	return date, nil
}

// parseDate reads an HTTP-date. A missing or unparsable one is the zero time,
// which every caller that needs the clock treats as "no clock".
func parseDate(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
