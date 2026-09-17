package zerops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// Membership is one entry of /user/info's clientUserList: the org and this
// identity's role in it.
type Membership struct {
	ID                string `json:"id"` // the clientUserId — what a project's userRoles names
	ClientID          string `json:"clientId"`
	RoleCode          string `json:"roleCode"`
	Status            string `json:"status"`
	CanCreateProjects bool   `json:"canCreateProjects"`
	CanViewFinances   bool   `json:"canViewFinances"`
	CanEditFinances   bool   `json:"canEditFinances"`
}

// UserInfo is what a token learns about itself. For an integration token, Id
// is the token's own id (ledger 2026-09-15) — which is what makes step 1 of
// the throwaway check possible.
type UserInfo struct {
	ID       string       `json:"id"`
	Email    string       `json:"email"`
	FullName string       `json:"fullName"`
	Clients  []Membership `json:"clientUserList"`
	APIDate  time.Time    `json:"-"`
}

// UserInfo is GET /user/info.
func (c *Client) UserInfo(ctx context.Context) (UserInfo, error) {
	var out UserInfo
	date, err := c.do(ctx, "GET", "/user/info", nil, &out)
	out.APIDate = date
	return out, err
}

// ---------------------------------------------------------------------------
// Integration tokens
// ---------------------------------------------------------------------------

// ProjectAccess is one project grant on an integration token.
type ProjectAccess struct {
	ProjectID string `json:"projectId"`
	RoleCode  string `json:"roleCode"`
}

// Token is an integration token's metadata. Its value is never part of it —
// only a freshly minted token carries one, in RawToken.
type Token struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	RoleCode          string          `json:"roleCode"`
	CanViewFinances   bool            `json:"canViewFinances"`
	CanEditFinances   bool            `json:"canEditFinances"`
	CanCreateProjects bool            `json:"canCreateProjects"`
	CreatedByUser     string          `json:"createdByUser"`
	Created           time.Time       `json:"created"`
	LastUpdate        time.Time       `json:"lastUpdate"`
	Projects          []ProjectAccess `json:"projects"`
	APIDate           time.Time       `json:"-"`
}

// TokenSpec is the body of a mint or an update.
type TokenSpec struct {
	Name              string          `json:"name"`
	RoleCode          string          `json:"roleCode"`
	CanViewFinances   bool            `json:"canViewFinances"`
	CanEditFinances   bool            `json:"canEditFinances"`
	CanCreateProjects bool            `json:"canCreateProjects"`
	Projects          []ProjectAccess `json:"projects"`
}

// RawToken is a minted token: the only response that carries a value.
type RawToken struct {
	Token
	Value string `json:"token"`
}

// IntegrationToken is GET /client/{org}/integration-token/{id}.
func (c *Client) IntegrationToken(ctx context.Context, clientID, tokenID string) (Token, error) {
	var out Token
	date, err := c.do(ctx, "GET", "/client/"+url.PathEscape(clientID)+"/integration-token/"+url.PathEscape(tokenID), nil, &out)
	out.APIDate = date
	return out, err
}

// TokenList is a page of the org's integration tokens.
type TokenList struct {
	Tokens  []Token   `json:"list"`
	APIDate time.Time `json:"-"`
}

// IntegrationTokens is GET /client/{org}/integration-token/list.
func (c *Client) IntegrationTokens(ctx context.Context, clientID string) (TokenList, error) {
	var out TokenList
	date, err := c.do(ctx, "GET", "/client/"+url.PathEscape(clientID)+"/integration-token/list", nil, &out)
	out.APIDate = date
	return out, err
}

// CreateIntegrationToken is POST /client/{org}/integration-token. The returned
// value is a live credential: it goes into the caller's answer or into a
// service variable, never into a log or an error.
func (c *Client) CreateIntegrationToken(ctx context.Context, clientID string, spec TokenSpec) (RawToken, error) {
	var out RawToken
	date, err := c.do(ctx, "POST", "/client/"+url.PathEscape(clientID)+"/integration-token", spec, &out)
	out.APIDate = date
	return out, err
}

// UpdateIntegrationToken is PUT /client/{org}/integration-token/{id}. Adding a
// project grant to the broker's own token is how a new environment becomes
// reachable — the value never changes, so no secret travels (ledger
// 2026-09-15).
func (c *Client) UpdateIntegrationToken(ctx context.Context, clientID, tokenID string, spec TokenSpec) (Token, error) {
	var out Token
	date, err := c.do(ctx, "PUT", "/client/"+url.PathEscape(clientID)+"/integration-token/"+url.PathEscape(tokenID), spec, &out)
	out.APIDate = date
	return out, err
}

// DeleteIntegrationToken is DELETE /client/{org}/integration-token/{id}.
func (c *Client) DeleteIntegrationToken(ctx context.Context, clientID, tokenID string) error {
	_, err := c.do(ctx, "DELETE", "/client/"+url.PathEscape(clientID)+"/integration-token/"+url.PathEscape(tokenID), nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// UserLight is the person behind a member row.
type UserLight struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"fullName"`
}

// Member is one row of the org's member list. ID is the clientUserId, which is
// what a project's userRoles names; UserID is the person (or, for an
// integration token, the token).
type Member struct {
	ID                string    `json:"id"`
	ClientID          string    `json:"clientId"`
	UserID            string    `json:"userId"`
	Status            string    `json:"status"`
	RoleCode          string    `json:"roleCode"`
	CanViewFinances   bool      `json:"canViewFinances"`
	CanEditFinances   bool      `json:"canEditFinances"`
	CanCreateProjects bool      `json:"canCreateProjects"`
	User              UserLight `json:"user"`
}

// MemberList is the whole member list. The API returns it unpaged.
type MemberList struct {
	Members []Member  `json:"clientUserList"`
	APIDate time.Time `json:"-"`
}

// Members is GET /client/{org}/user/list. Every token, whatever its role, can
// read it (ledger 2026-09-15).
func (c *Client) Members(ctx context.Context, clientID string) (MemberList, error) {
	var out MemberList
	date, err := c.do(ctx, "GET", "/client/"+url.PathEscape(clientID)+"/user/list", nil, &out)
	out.APIDate = date
	return out, err
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

// UserRole is one per-project override: a clientUserId and the role it carries
// there.
type UserRole struct {
	ClientUserID string `json:"clientUserId"`
	RoleCode     string `json:"roleCode"`
}

// ProjectEnv is one project-level variable.
type ProjectEnv struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Key       string `json:"key"`
	Content   string `json:"content"`
	Type      string `json:"type"`
	Sensitive bool   `json:"sensitive"`
}

// Project is a Zerops project as both GET /project/{id} and
// POST /project/search return it. EnvList is populated by the search only.
type Project struct {
	ID                  string       `json:"id"`
	ClientID            string       `json:"clientId"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	Status              string       `json:"status"`
	TagList             []string     `json:"tagList"`
	UserRoles           []UserRole   `json:"userRoles"`
	EnvList             []ProjectEnv `json:"envList"`
	PublicIpV4Shared    bool         `json:"publicIpV4Shared"`
	ZeropsSubdomainHost string       `json:"zeropsSubdomainHost"`
	APIDate             time.Time    `json:"-"`
}

// Project is GET /project/{id}, whose userRoles a READ_ONLY token can read
// (ledger 2026-09-15).
func (c *Client) Project(ctx context.Context, projectID string) (Project, error) {
	var out Project
	date, err := c.do(ctx, "GET", "/project/"+url.PathEscape(projectID), nil, &out)
	out.APIDate = date
	return out, err
}

// searchTerm is one clause of the platform's ES filter.
type searchTerm struct {
	Name     string `json:"name"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type searchFilter struct {
	Search []searchTerm `json:"search"`
	Sort   []struct{}   `json:"sort"`
	Limit  int          `json:"limit,omitempty"`
	Offset int          `json:"offset,omitempty"`
}

// ProjectPage is one page of POST /project/search. Total is the platform's
// declared totalHits: a page shorter than it, with no further page fetched, is
// a partial read, and the rights loop refuses to write on one.
type ProjectPage struct {
	Projects []Project `json:"items"`
	Total    int       `json:"totalHits"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
	APIDate  time.Time `json:"-"`
}

// searchPageSize is what one POST /project/search asks for.
const searchPageSize = 200

// SearchProjects reads every project of the org, following the platform's
// paging until it has the declared total. It is the call that carries tagList,
// userRoles and envList in one read.
//
// ErrPartial is returned when the pages do not add up to totalHits: the caller
// must treat that as "the org is unreadable", never as "the org shrank".
func (c *Client) SearchProjects(ctx context.Context, clientID string) (ProjectPage, error) {
	var all ProjectPage
	offset := 0
	for {
		filter := searchFilter{
			Search: []searchTerm{{Name: "clientId", Operator: "eq", Value: clientID}},
			Sort:   []struct{}{},
			Limit:  searchPageSize,
			Offset: offset,
		}
		var page ProjectPage
		date, err := c.do(ctx, "POST", "/project/search", filter, &page)
		if err != nil {
			return all, err
		}
		all.APIDate = date
		all.Total = page.Total
		all.Projects = append(all.Projects, page.Projects...)
		if len(page.Projects) == 0 || len(all.Projects) >= page.Total {
			break
		}
		offset += len(page.Projects)
	}
	if len(all.Projects) != all.Total {
		return all, fmt.Errorf("%w: project search returned %d of %d", ErrPartial, len(all.Projects), all.Total)
	}
	return all, nil
}

// ErrPartial marks a list read that came back short of its own declared total.
var ErrPartial = errors.New("partial read")

// IsPartial reports whether err is a partial read.
func IsPartial(err error) bool { return errors.Is(err, ErrPartial) }

// ProjectUpdate is the body of PUT /project/{id}. userRoles is deliberately
// absent: docs/vocabulary.md and the ledger both say it is an object on the
// wire that must be omitted, and sending it would rewrite the project's
// per-person overrides.
type ProjectUpdate struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	TagList          []string `json:"tagList"`
	PublicIpV4Shared bool     `json:"publicIpV4Shared"`
	MaxCreditLimit   *float64 `json:"maxCreditLimit"`
}

// UpdateProject is PUT /project/{id} — the registry write. It needs effective
// OWNER or ADMIN; the broker's own token is org READ_ONLY, so this is the app's
// call, not the loop's.
func (c *Client) UpdateProject(ctx context.Context, projectID string, update ProjectUpdate) error {
	if update.TagList == nil {
		update.TagList = []string{}
	}
	_, err := c.do(ctx, "PUT", "/project/"+url.PathEscape(projectID), update, nil)
	return err
}

// ---------------------------------------------------------------------------
// Project variables
// ---------------------------------------------------------------------------

// ProjectEnvs reads one project's variables. They arrive on the search, with
// the ids PUT and DELETE need (ledger 2026-09-16).
func (c *Client) ProjectEnvs(ctx context.Context, projectID string) ([]ProjectEnv, error) {
	filter := searchFilter{
		Search: []searchTerm{{Name: "id", Operator: "eq", Value: projectID}},
		Sort:   []struct{}{},
		Limit:  1,
	}
	var page ProjectPage
	if _, err := c.do(ctx, "POST", "/project/search", filter, &page); err != nil {
		return nil, err
	}
	if len(page.Projects) == 0 {
		return nil, &APIError{Status: 404, Code: "projectNotFound", Message: "no such project"}
	}
	return page.Projects[0].EnvList, nil
}

// EnvSpec is the body of a project-variable create or update.
type EnvSpec struct {
	Key       string `json:"key"`
	Content   string `json:"content"`
	Sensitive bool   `json:"sensitive"`
}

// CreateProjectEnv is POST /project/{id}/env.
func (c *Client) CreateProjectEnv(ctx context.Context, projectID string, spec EnvSpec) (ProjectEnv, error) {
	var out ProjectEnv
	_, err := c.do(ctx, "POST", "/project/"+url.PathEscape(projectID)+"/env", spec, &out)
	return out, err
}

// UpdateProjectEnv is PUT /project-env/{id}.
func (c *Client) UpdateProjectEnv(ctx context.Context, envID string, spec EnvSpec) (ProjectEnv, error) {
	var out ProjectEnv
	_, err := c.do(ctx, "PUT", "/project-env/"+url.PathEscape(envID), spec, &out)
	return out, err
}

// DeleteProjectEnv is DELETE /project-env/{id}.
func (c *Client) DeleteProjectEnv(ctx context.Context, envID string) error {
	_, err := c.do(ctx, "DELETE", "/project-env/"+url.PathEscape(envID), nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Services
// ---------------------------------------------------------------------------

// Service is a service stack, as the search returns it. Name is the hostname
// — the name an environment's recipe and a deploy request both use.
type Service struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	ClientID  string `json:"clientId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Type      string `json:"serviceStackTypeId"`
	// TypeInfo names the type and version the service runs, as the search
	// carries it. A Mate's container is the service whose version name starts
	// with zcp@.
	TypeInfo ServiceTypeInfo `json:"serviceStackTypeInfo"`
	// IsSystem marks a stack the platform owns rather than a person: every
	// project's `core`, and the transient build and prepare stacks a deploy
	// makes. Nothing that compares a project against a recipe may see one.
	IsSystem bool `json:"isSystem"`
	// SubdomainAccess says whether the service already answers on its
	// zerops.app host. Enabling it is a post-deploy call, so the broker only
	// makes it when this is false.
	SubdomainAccess bool `json:"subdomainAccess"`
	// Ports is what the service listens on; one with HTTPRouting is what makes
	// a subdomain meaningful.
	Ports []ServicePort `json:"ports"`
}

// ServiceTypeInfo is the search's serviceStackTypeInfo: which type, at which
// version, a service runs.
type ServiceTypeInfo struct {
	VersionName string `json:"serviceStackTypeVersionName"`
}

// ServicePort is one port of a service.
type ServicePort struct {
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Scheme      string `json:"scheme"`
	HTTPRouting bool   `json:"httpRouting"`
}

// HTTP reports whether the service serves HTTP, which is what makes
// enable-subdomain-access anything but a 400 serviceStackIsNotHttp.
//
// Two shapes answer this, and only one of them carries the routing flags:
// POST /service-stack/search returns `httpRouting` and `portRouting`, while
// GET /service-stack/{id} — the read the executor makes before it publishes a
// service — returns the port's `scheme` and no flags at all (measured
// 2026-09-16). So the scheme decides when the flag is absent.
func (s Service) HTTP() bool {
	for _, p := range s.Ports {
		if p.HTTPRouting || p.Scheme == "http" || p.Scheme == "https" {
			return true
		}
	}
	return false
}

// WithoutSystem drops the platform's own stacks from a service list. Every
// reader that compares a project against something a person wrote — a recipe,
// the registry — starts here: `core` is on every project and a build stack
// lives for the length of a deploy, so neither is ever a service somebody
// declared and then removed (ledger 2026-09-16).
func WithoutSystem(services []Service) []Service {
	out := make([]Service, 0, len(services))
	for _, service := range services {
		if service.IsSystem {
			continue
		}
		out = append(out, service)
	}
	return out
}

type servicePage struct {
	Items  []Service `json:"items"`
	Total  int       `json:"totalHits"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// Services lists one project's services with POST /service-stack/search.
func (c *Client) Services(ctx context.Context, clientID, projectID string) ([]Service, error) {
	var all []Service
	offset := 0
	for {
		filter := searchFilter{
			Search: []searchTerm{
				{Name: "clientId", Operator: "eq", Value: clientID},
				{Name: "projectId", Operator: "eq", Value: projectID},
			},
			Sort:   []struct{}{},
			Limit:  searchPageSize,
			Offset: offset,
		}
		var page servicePage
		if _, err := c.do(ctx, "POST", "/service-stack/search", filter, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if len(page.Items) == 0 || len(all) >= page.Total {
			if len(all) != page.Total {
				return all, fmt.Errorf("%w: service search returned %d of %d", ErrPartial, len(all), page.Total)
			}
			break
		}
		offset += len(page.Items)
	}
	return all, nil
}

// ImportResult is what POST /project/{id}/service-stack/import answers: the
// services it created and the processes that build them.
type ImportResult struct {
	ServiceStacks []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Processes []struct {
			ID string `json:"id"`
		} `json:"processes"`
	} `json:"serviceStacks"`
	APIDate time.Time `json:"-"`
}

// ImportServices is POST /project/{id}/service-stack/import — the services-only
// import, with no project block. A BASIC_USER on the project may run it
// (ledger 2026-09-15).
func (c *Client) ImportServices(ctx context.Context, projectID, yaml string) (ImportResult, error) {
	var out ImportResult
	date, err := c.do(ctx, "POST", "/project/"+url.PathEscape(projectID)+"/service-stack/import",
		map[string]string{"yaml": yaml}, &out)
	out.APIDate = date
	return out, err
}

// StopService is PUT /service-stack/{id}/stop. BASIC_USER on the project is
// enough, and the platform settles in about six seconds (ledger 2026-09-16).
// A stopped service has startOnProjectStart false, so nothing but the broker
// wakes it.
func (c *Client) StopService(ctx context.Context, serviceID string) error {
	_, err := c.do(ctx, "PUT", "/service-stack/"+url.PathEscape(serviceID)+"/stop", nil, nil)
	return err
}

// StartService is PUT /service-stack/{id}/start.
func (c *Client) StartService(ctx context.Context, serviceID string) error {
	_, err := c.do(ctx, "PUT", "/service-stack/"+url.PathEscape(serviceID)+"/start", nil, nil)
	return err
}
