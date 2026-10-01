package zerops

import (
	"context"
	"fmt"
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
// with the ids an update needs. The platform pages it, 20 a page by default,
// and a zcp container holds more (29 measured 2026-09-30), so every page is
// read. A read short of its declared total is ErrPartial: a variable missing
// from it would be created again and refused as a duplicate.
func (c *Client) UserData(ctx context.Context, serviceID string) ([]ServiceUserData, error) {
	var all []ServiceUserData
	for {
		var page struct {
			List  []ServiceUserData `json:"list"`
			Total int               `json:"total"`
		}
		path := fmt.Sprintf("/service-stack/%s/user-data?limit=%d&offset=%d", url.PathEscape(serviceID), userDataPageSize, len(all))
		if _, err := c.do(ctx, "GET", path, nil, &page); err != nil {
			return all, err
		}
		all = append(all, page.List...)
		if len(page.List) == 0 || len(all) >= page.Total {
			if len(all) < page.Total {
				return all, fmt.Errorf("%w: user data returned %d of %d", ErrPartial, len(all), page.Total)
			}
			return all, nil
		}
	}
}

const userDataPageSize = 100

// UserDataSpec is the body of a service-variable create.
type UserDataSpec struct {
	Key       string `json:"key"`
	Content   string `json:"content"`
	Sensitive bool   `json:"sensitive"`
}

// CodeUserDataDuplicateKey is the platform's refusal of a create whose key
// the service holds already, compared in any case (measured 2026-09-30:
// "Service environment variable key 'GITEA_URL' is not unique (case
// insensitive)"). A variable that exists is updated by its id instead.
const CodeUserDataDuplicateKey = "userDataDuplicateKey"

// CreateUserData is POST /service-stack/{id}/user-data. It answers a process,
// which the caller may wait on and the rights loop does not: the value is
// present within seconds either way.
func (c *Client) CreateUserData(ctx context.Context, serviceID string, spec UserDataSpec) (Process, error) {
	var out Process
	_, err := c.do(ctx, "POST", "/service-stack/"+url.PathEscape(serviceID)+"/user-data", spec, &out)
	return out, err
}

// DeleteUserData is DELETE /user-data/{id}. It answers a process, which the
// rights loop does not wait on.
func (c *Client) DeleteUserData(ctx context.Context, id string) error {
	_, err := c.do(ctx, "DELETE", "/user-data/"+url.PathEscape(id), nil, nil)
	return err
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
