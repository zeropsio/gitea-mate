package gitea

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// AttachmentPattern is how Gitea names an attachment: a lowercase UUID. The
// broker refuses anything else before it asks Gitea.
var AttachmentPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// AttachmentBody is one attachment's bytes as Gitea serves them. The caller
// closes Body.
type AttachmentBody struct {
	Body io.ReadCloser
	// ContentType is Gitea's word for the bytes, detected from them.
	ContentType string
	// ContentLength is -1 when Gitea did not say.
	ContentLength int64
}

// Attachment is GET /attachments/{uuid}, as the client's token — Gitea's web
// route, and the only one that serves an attachment's bytes: every API route
// under .../assets answers JSON about it (measured on Gitea 1.27.2).
//
// Gitea answers a token it does not accept on this route with a 303 to its
// sign-in page (REQUIRE_SIGNIN_VIEW), and followed, that is a 200 of HTML. The
// redirect is never followed: it is read as the refusal it is, a 401. A token
// without read:issue is Gitea's 403, and an attachment the token may not read
// is its 404, exactly as one that does not exist.
func (c *Client) Attachment(ctx context.Context, uuid string) (*AttachmentBody, error) {
	path := "/attachments/" + uuid
	if !AttachmentPattern.MatchString(uuid) {
		return nil, &APIError{Status: http.StatusBadRequest, Message: "not an attachment's id", Path: path}
	}
	creds, err := c.admin.Admin(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitea: GET %s: the caller's token: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("gitea: GET %s: %w", path, err)
	}
	req.Header.Set("Authorization", "token "+creds.Token)

	noRedirects := *c.http
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirects.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea: GET %s: %w", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return &AttachmentBody{
			Body:          resp.Body,
			ContentType:   resp.Header.Get("Content-Type"),
			ContentLength: resp.ContentLength,
		}, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		_ = resp.Body.Close()
		return nil, &APIError{Status: http.StatusUnauthorized, Message: "Gitea asked for a sign-in: it did not accept the token", Path: path}
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode), Path: path}
	}
}
