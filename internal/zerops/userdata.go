package zerops

import (
	"context"
	"net/url"
)

// ---------------------------------------------------------------------------
// Service variables
// ---------------------------------------------------------------------------
//
// The three calls the rights loop delivers a Mate's Gitea access with. Shapes
// measured 2026-09-16 and 2026-09-17: a sensitive value comes back in clear to
// a BASIC_USER token on the project; a create is accepted on a service that is
// still NEW or READY_TO_DEPLOY and is present once it is ACTIVE; the platform
// rewrites the container's live env store within seconds of a write, so no
// restart follows one.

// UserData is GET /service-stack/{id}/user-data: the service's own variables,
// with the ids an update needs.
func (c *Client) UserData(ctx context.Context, serviceID string) ([]ServiceUserData, error) {
	var out struct {
		List []ServiceUserData `json:"list"`
	}
	_, err := c.do(ctx, "GET", "/service-stack/"+url.PathEscape(serviceID)+"/user-data", nil, &out)
	return out.List, err
}

// UserDataSpec is the body of a service-variable create.
type UserDataSpec struct {
	Key       string `json:"key"`
	Content   string `json:"content"`
	Sensitive bool   `json:"sensitive"`
}

// CreateUserData is POST /service-stack/{id}/user-data. It answers a process,
// which the caller may wait on and the rights loop does not: the value is
// present within seconds either way.
func (c *Client) CreateUserData(ctx context.Context, serviceID string, spec UserDataSpec) (Process, error) {
	var out Process
	_, err := c.do(ctx, "POST", "/service-stack/"+url.PathEscape(serviceID)+"/user-data", spec, &out)
	return out, err
}

// UpdateUserData is PUT /user-data/{id}. The body carries the key beside the
// content — the platform refuses one without it (measured 2026-09-17).
func (c *Client) UpdateUserData(ctx context.Context, id, key, content string) error {
	body := struct {
		Key     string `json:"key"`
		Content string `json:"content"`
	}{Key: key, Content: content}
	_, err := c.do(ctx, "PUT", "/user-data/"+url.PathEscape(id), body, nil)
	return err
}
