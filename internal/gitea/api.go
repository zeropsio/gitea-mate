package gitea

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// User is a Gitea user, person or bot.
type User struct {
	ID         int64  `json:"id"`
	Login      string `json:"login"`
	LoginName  string `json:"login_name"`
	SourceID   int64  `json:"source_id"`
	FullName   string `json:"full_name"`
	Email      string `json:"email"`
	IsAdmin    bool   `json:"is_admin"`
	Restricted bool   `json:"restricted"`
	Active     bool   `json:"active"`
	Visibility string `json:"visibility"`
}

// NewUser creates a user. Its password is generated here, never returned and
// never logged: a bot signs in with a token alone, and a person arrives through
// the OIDC source.
type NewUser struct {
	Login    string
	Email    string
	FullName string
	// Restricted hides even public repositories from the user, which is what a
	// Mate's bot gets.
	Restricted bool
	Visibility string
	// SourceID binds the account to a login source — the OIDC source, for a
	// person — and LoginName is the identity that source knows them by (the
	// OIDC `sub`). Gitea's *Sign in with Zerops* looks an account up by exactly
	// this pair before it tries to register or link one, so a person the broker
	// created signs in to Gitea's own pages as the same account. Zero means a
	// local account, which needs a password.
	SourceID  int64
	LoginName string
}

type createUserOption struct {
	Username           string `json:"username"`
	Email              string `json:"email"`
	FullName           string `json:"full_name,omitempty"`
	Password           string `json:"password,omitempty"`
	MustChangePassword bool   `json:"must_change_password"`
	Restricted         bool   `json:"restricted"`
	Visibility         string `json:"visibility,omitempty"`
	SendNotify         bool   `json:"send_notify"`
	SourceID           int64  `json:"source_id,omitempty"`
	LoginName          string `json:"login_name,omitempty"`
}

// CreateUser is POST /admin/users.
//
// Measured on Gitea 1.27.2 (2026-09-17): an account with no source needs a
// password (`400 PasswordIsRequired`); one bound to an OAuth2 source with a
// `login_name` is created without one, active, and the site admin's basic
// auth mints it tokens like any other user's.
func (c *Client) CreateUser(ctx context.Context, u NewUser) (User, error) {
	in := createUserOption{
		Username:           u.Login,
		Email:              u.Email,
		FullName:           u.FullName,
		MustChangePassword: false,
		Restricted:         u.Restricted,
		Visibility:         u.Visibility,
		SourceID:           u.SourceID,
		LoginName:          u.LoginName,
	}
	if u.SourceID == 0 {
		password, err := randomPassword()
		if err != nil {
			return User{}, err
		}
		in.Password = password
	}
	var out User
	err := c.do(ctx, http.MethodPost, "/admin/users", in, &out, authToken)
	return out, err
}

// UserEdit is the subset of PATCH /admin/users/{login} the broker writes. A nil
// field is left alone.
type UserEdit struct {
	Active                  *bool
	Restricted              *bool
	MaxRepoCreation         *int
	AllowCreateOrganization *bool
	FullName                *string
	Admin                   *bool
}

type editUserOption struct {
	// Gitea's EditUserOption requires login_name and source_id on the wire;
	// sending the login itself with source 0 keeps a local user local.
	LoginName               string  `json:"login_name"`
	SourceID                int64   `json:"source_id"`
	Active                  *bool   `json:"active,omitempty"`
	Restricted              *bool   `json:"restricted,omitempty"`
	MaxRepoCreation         *int    `json:"max_repo_creation,omitempty"`
	AllowCreateOrganization *bool   `json:"allow_create_organization,omitempty"`
	FullName                *string `json:"full_name,omitempty"`
	Admin                   *bool   `json:"admin,omitempty"`
}

// EditUser is PATCH /admin/users/{login}.
func (c *Client) EditUser(ctx context.Context, login string, e UserEdit) (User, error) {
	in := editUserOption{
		LoginName:               login,
		Active:                  e.Active,
		Restricted:              e.Restricted,
		MaxRepoCreation:         e.MaxRepoCreation,
		AllowCreateOrganization: e.AllowCreateOrganization,
		FullName:                e.FullName,
		Admin:                   e.Admin,
	}
	var out User
	err := c.do(ctx, http.MethodPatch, "/admin/users/"+esc(login), in, &out, authToken)
	return out, err
}

// GetUser is GET /users/{login}.
func (c *Client) GetUser(ctx context.Context, login string) (User, error) {
	var out User
	err := c.do(ctx, http.MethodGet, "/users/"+esc(login), nil, &out, authToken)
	return out, err
}

// ListUsers is GET /admin/users — every account Gitea holds.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var all []User
	err := paged(func(page int) (int, error) {
		var out []User
		if err := c.do(ctx, http.MethodGet, withPage("/admin/users", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// WhoAmI is GET /user as whatever token this client holds. With a
// write:repository,read:user token it answers the bot (measured on 1.27.2:
// write:repository alone is 403, naming read:user as required).
func (c *Client) WhoAmI(ctx context.Context) (User, error) {
	var out User
	err := c.do(ctx, http.MethodGet, "/user", nil, &out, authToken)
	return out, err
}

// ---------------------------------------------------------------------------
// Tokens — basic auth only
// ---------------------------------------------------------------------------

// AccessToken is one of a user's API tokens. Value is set only by MintToken.
//
// CreatedAt is real on the list route and zero on the create response
// (measured on 1.27.2), which is why the rights loop reads a generation's age
// from a list and never from the answer that minted it.
type AccessToken struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Scopes         []string  `json:"scopes"`
	TokenLastEight string    `json:"token_last_eight"`
	CreatedAt      time.Time `json:"created_at"`
	Value          string    `json:"sha1"`
}

// MintToken is POST /users/{login}/tokens: the site admin mints a token for
// another user. Measured on Gitea 1.27.2 — this route refuses an API token
// (401 "auth required") and takes the site admin's basic auth; the Sudo header
// is not needed.
//
// The returned Value is a live credential: it goes straight into the answer the
// app writes onto the Mate's service, and nowhere else.
func (c *Client) MintToken(ctx context.Context, login, name string, scopes []string) (AccessToken, error) {
	in := struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}{Name: name, Scopes: scopes}
	var out AccessToken
	err := c.do(ctx, http.MethodPost, "/users/"+esc(login)+"/tokens", in, &out, authBasic)
	return out, err
}

// ListTokens is GET /users/{login}/tokens. Basic auth, like every token route.
// The values are never returned by Gitea.
func (c *Client) ListTokens(ctx context.Context, login string) ([]AccessToken, error) {
	var all []AccessToken
	err := paged(func(page int) (int, error) {
		var out []AccessToken
		if err := c.do(ctx, http.MethodGet, withPage("/users/"+esc(login)+"/tokens", page), nil, &out, authBasic); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// DeleteToken is DELETE /users/{login}/tokens/{name}. The path segment takes
// either an id or a name; a name with slashes (mate/{bot}/{n}) must be escaped.
func (c *Client) DeleteToken(ctx context.Context, login, name string) error {
	return c.do(ctx, http.MethodDelete, "/users/"+esc(login)+"/tokens/"+esc(name), nil, nil, authBasic)
}

// randomPassword makes a password nobody ever sees. A bot has no interactive
// login and a person arrives through OIDC, so this value exists only because
// Gitea insists on one.
func randomPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// Gitea's default password policy wants mixed classes; base64 plus a fixed
	// punctuation tail satisfies it without weakening the 256 random bits.
	return base64.RawURLEncoding.EncodeToString(raw) + "aA1!", nil
}

// ---------------------------------------------------------------------------
// Organisations
// ---------------------------------------------------------------------------

// Org is a Gitea organisation: one per registered group, named by its slug.
type Org struct {
	ID         int64  `json:"id"`
	Name       string `json:"username"`
	FullName   string `json:"full_name"`
	Visibility string `json:"visibility"`
}

// CreateOrg is POST /orgs.
func (c *Client) CreateOrg(ctx context.Context, name, fullName string) (Org, error) {
	in := struct {
		Username               string `json:"username"`
		FullName               string `json:"full_name,omitempty"`
		Visibility             string `json:"visibility"`
		RepoAdminChangeTeamAcc bool   `json:"repo_admin_change_team_access"`
	}{Username: name, FullName: fullName, Visibility: "private"}
	var out Org
	err := c.do(ctx, http.MethodPost, "/orgs", in, &out, authToken)
	return out, err
}

// GetOrg is GET /orgs/{org}.
func (c *Client) GetOrg(ctx context.Context, name string) (Org, error) {
	var out Org
	err := c.do(ctx, http.MethodGet, "/orgs/"+esc(name), nil, &out, authToken)
	return out, err
}

// IsOrgMember is GET /orgs/{org}/members/{login}: 204 yes, 404 no.
func (c *Client) IsOrgMember(ctx context.Context, org, login string) (bool, error) {
	return c.exists(ctx, "/orgs/"+esc(org)+"/members/"+esc(login))
}

// RemoveOrgMember is DELETE /orgs/{org}/members/{login}.
func (c *Client) RemoveOrgMember(ctx context.Context, org, login string) error {
	return c.do(ctx, http.MethodDelete, "/orgs/"+esc(org)+"/members/"+esc(login), nil, nil, authToken)
}

// ---------------------------------------------------------------------------
// Teams
// ---------------------------------------------------------------------------

// Team is one of a group's three teams: read, write, release.
type Team struct {
	ID                      int64             `json:"id"`
	Name                    string            `json:"name"`
	Permission              string            `json:"permission"`
	IncludesAllRepositories bool              `json:"includes_all_repositories"`
	UnitsMap                map[string]string `json:"units_map"`
}

// NewTeam is what CreateTeam sends.
type NewTeam struct {
	Name string
	// Permission is read, write or admin.
	Permission string
	// UnitsMap names each repository unit's permission. Gitea's `units` array
	// is deprecated in favour of it.
	UnitsMap map[string]string
}

// AllRepoUnits is every repository unit at one permission — "units: all
// repositories" of docs/vocabulary.md.
func AllRepoUnits(permission string) map[string]string {
	units := []string{
		"repo.code", "repo.issues", "repo.pulls", "repo.releases",
		"repo.wiki", "repo.projects", "repo.packages", "repo.actions",
	}
	m := make(map[string]string, len(units))
	for _, u := range units {
		m[u] = permission
	}
	return m
}

// CreateTeam is POST /orgs/{org}/teams.
func (c *Client) CreateTeam(ctx context.Context, org string, t NewTeam) (Team, error) {
	in := struct {
		Name                    string            `json:"name"`
		Permission              string            `json:"permission"`
		IncludesAllRepositories bool              `json:"includes_all_repositories"`
		CanCreateOrgRepo        bool              `json:"can_create_org_repo"`
		UnitsMap                map[string]string `json:"units_map"`
	}{
		Name:                    t.Name,
		Permission:              t.Permission,
		IncludesAllRepositories: true,
		CanCreateOrgRepo:        false,
		UnitsMap:                t.UnitsMap,
	}
	var out Team
	err := c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/teams", in, &out, authToken)
	return out, err
}

// ListTeams is GET /orgs/{org}/teams.
func (c *Client) ListTeams(ctx context.Context, org string) ([]Team, error) {
	var all []Team
	err := paged(func(page int) (int, error) {
		var out []Team
		if err := c.do(ctx, http.MethodGet, withPage("/orgs/"+esc(org)+"/teams", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// ListTeamMembers is GET /teams/{id}/members.
func (c *Client) ListTeamMembers(ctx context.Context, teamID int64) ([]User, error) {
	var all []User
	err := paged(func(page int) (int, error) {
		var out []User
		if err := c.do(ctx, http.MethodGet, withPage("/teams/"+itoa(teamID)+"/members", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// AddTeamMember is PUT /teams/{id}/members/{login}.
func (c *Client) AddTeamMember(ctx context.Context, teamID int64, login string) error {
	return c.do(ctx, http.MethodPut, "/teams/"+itoa(teamID)+"/members/"+esc(login), nil, nil, authToken)
}

// RemoveTeamMember is DELETE /teams/{id}/members/{login}.
func (c *Client) RemoveTeamMember(ctx context.Context, teamID int64, login string) error {
	return c.do(ctx, http.MethodDelete, "/teams/"+itoa(teamID)+"/members/"+esc(login), nil, nil, authToken)
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

// Repo is a Gitea repository.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Empty         bool   `json:"empty"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
}

// NewRepo is what CreateOrgRepo sends. Every repository the broker makes is
// private, initialised (so `main` exists and can be protected) and defaults to
// `main`.
type NewRepo struct {
	Name        string
	Description string
}

// CreateOrgRepo is POST /orgs/{org}/repos.
func (c *Client) CreateOrgRepo(ctx context.Context, org string, r NewRepo) (Repo, error) {
	in := struct {
		Name          string `json:"name"`
		Description   string `json:"description,omitempty"`
		Private       bool   `json:"private"`
		AutoInit      bool   `json:"auto_init"`
		DefaultBranch string `json:"default_branch"`
		Readme        string `json:"readme"`
	}{Name: r.Name, Description: r.Description, Private: true, AutoInit: true, DefaultBranch: "main", Readme: "Default"}
	var out Repo
	err := c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/repos", in, &out, authToken)
	return out, err
}

// GetRepo is GET /repos/{owner}/{repo}.
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (Repo, error) {
	var out Repo
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo), nil, &out, authToken)
	return out, err
}

// ListOrgRepos is GET /orgs/{org}/repos.
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]Repo, error) {
	var all []Repo
	err := paged(func(page int) (int, error) {
		var out []Repo
		if err := c.do(ctx, http.MethodGet, withPage("/orgs/"+esc(org)+"/repos", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// AddCollaborator is PUT /repos/{o}/{r}/collaborators/{login}. A bot writes as
// a collaborator on its group's service repositories, and nowhere else.
func (c *Client) AddCollaborator(ctx context.Context, owner, repo, login, permission string) error {
	in := struct {
		Permission string `json:"permission"`
	}{Permission: permission}
	return c.do(ctx, http.MethodPut, "/repos/"+esc(owner)+"/"+esc(repo)+"/collaborators/"+esc(login), in, nil, authToken)
}

// IsCollaborator is GET /repos/{o}/{r}/collaborators/{login}: 204 yes, 404 no.
func (c *Client) IsCollaborator(ctx context.Context, owner, repo, login string) (bool, error) {
	return c.exists(ctx, "/repos/"+esc(owner)+"/"+esc(repo)+"/collaborators/"+esc(login))
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// PullRequest is what the broker needs of one: who opened it, where it lands,
// whether it is still open.
type PullRequest struct {
	Number int64             `json:"number"`
	State  string            `json:"state"`
	Merged bool              `json:"merged"`
	Title  string            `json:"title"`
	User   PullRequestUser   `json:"user"`
	Base   PullRequestBranch `json:"base"`
	Head   PullRequestBranch `json:"head"`
}

// PullRequestUser is the account that opened a pull request.
type PullRequestUser struct {
	Login string `json:"login"`
}

// PullRequestBranch is one side of a pull request.
type PullRequestBranch struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

// ListPullRequests is GET /repos/{o}/{r}/pulls?state={state}, every page.
func (c *Client) ListPullRequests(ctx context.Context, owner, repo, state string) ([]PullRequest, error) {
	var all []PullRequest
	err := paged(func(page int) (int, error) {
		var out []PullRequest
		path := withPage("/repos/"+esc(owner)+"/"+esc(repo)+"/pulls?state="+esc(state), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// PullRequestFile is one file a pull request changes.
type PullRequestFile struct {
	Filename string `json:"filename"`
}

// PullRequestFiles is GET /repos/{o}/{r}/pulls/{n}/files, every page. None at
// all is a request Gitea calls empty: its branch carries nothing the base
// lacks, and no merge will ever be accepted.
func (c *Client) PullRequestFiles(ctx context.Context, owner, repo string, number int64) ([]PullRequestFile, error) {
	var all []PullRequestFile
	err := paged(func(page int) (int, error) {
		var out []PullRequestFile
		path := withPage("/repos/"+esc(owner)+"/"+esc(repo)+"/pulls/"+strconv.FormatInt(number, 10)+"/files", page)
		if err := c.do(ctx, http.MethodGet, path, nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// ClosePullRequest is PATCH /repos/{o}/{r}/pulls/{n} with state closed.
func (c *Client) ClosePullRequest(ctx context.Context, owner, repo string, number int64) error {
	in := struct {
		State string `json:"state"`
	}{State: "closed"}
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/pulls/" + strconv.FormatInt(number, 10)
	return c.do(ctx, http.MethodPatch, path, in, nil, authToken)
}

// MergePullRequest is POST /repos/{o}/{r}/pulls/{n}/merge with a merge commit.
// Gitea answers 405 when the request cannot be merged as it is.
func (c *Client) MergePullRequest(ctx context.Context, owner, repo string, number int64, message string) error {
	in := struct {
		Do      string `json:"Do"`
		Message string `json:"merge_message_field,omitempty"`
	}{Do: "merge", Message: message}
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/pulls/" + strconv.FormatInt(number, 10) + "/merge"
	return c.do(ctx, http.MethodPost, path, in, nil, authToken)
}

// ---------------------------------------------------------------------------
// Branch and tag protection
// ---------------------------------------------------------------------------

// BranchProtection is one rule. RuleName is a glob and may name a branch that
// does not exist yet — which is how `env/*` is closed before the broker has
// written anything to it.
type BranchProtection struct {
	RuleName string `json:"rule_name"`

	EnablePush          bool     `json:"enable_push"`
	EnablePushWhitelist bool     `json:"enable_push_whitelist"`
	PushWhitelistTeams  []string `json:"push_whitelist_teams"`
	PushWhitelistUsers  []string `json:"push_whitelist_usernames"`

	EnableMergeWhitelist bool     `json:"enable_merge_whitelist"`
	MergeWhitelistTeams  []string `json:"merge_whitelist_teams"`
	MergeWhitelistUsers  []string `json:"merge_whitelist_usernames"`

	BlockAdminMergeOverride bool `json:"block_admin_merge_override"`
	EnableForcePush         bool `json:"enable_force_push"`
	RequiredApprovals       int  `json:"required_approvals"`
}

// CreateBranchProtection is POST /repos/{o}/{r}/branch_protections.
func (c *Client) CreateBranchProtection(ctx context.Context, owner, repo string, p BranchProtection) (BranchProtection, error) {
	var out BranchProtection
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(owner)+"/"+esc(repo)+"/branch_protections", p, &out, authToken)
	return out, err
}

// ListBranchProtections is GET /repos/{o}/{r}/branch_protections.
func (c *Client) ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error) {
	var out []BranchProtection
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/branch_protections", nil, &out, authToken)
	return out, err
}

// EditBranchProtection is PATCH /repos/{o}/{r}/branch_protections/{name}.
func (c *Client) EditBranchProtection(ctx context.Context, owner, repo, name string, p BranchProtection) (BranchProtection, error) {
	var out BranchProtection
	err := c.do(ctx, http.MethodPatch, "/repos/"+esc(owner)+"/"+esc(repo)+"/branch_protections/"+esc(name), p, &out, authToken)
	return out, err
}

// TagProtection is a `v*` rule: which teams may create a matching tag. Gitea
// gives protected tags no admin override (ledger 2026-09-15), so this is the
// hardest gate in the account.
type TagProtection struct {
	ID             int64    `json:"id,omitempty"`
	NamePattern    string   `json:"name_pattern"`
	WhitelistTeams []string `json:"whitelist_teams"`
	WhitelistUsers []string `json:"whitelist_usernames"`
}

// CreateTagProtection is POST /repos/{o}/{r}/tag_protections.
func (c *Client) CreateTagProtection(ctx context.Context, owner, repo string, p TagProtection) (TagProtection, error) {
	var out TagProtection
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(owner)+"/"+esc(repo)+"/tag_protections", p, &out, authToken)
	return out, err
}

// ListTagProtections is GET /repos/{o}/{r}/tag_protections.
func (c *Client) ListTagProtections(ctx context.Context, owner, repo string) ([]TagProtection, error) {
	var out []TagProtection
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/tag_protections", nil, &out, authToken)
	return out, err
}

// EditTagProtection is PATCH /repos/{o}/{r}/tag_protections/{id}.
func (c *Client) EditTagProtection(ctx context.Context, owner, repo string, id int64, p TagProtection) (TagProtection, error) {
	var out TagProtection
	err := c.do(ctx, http.MethodPatch, "/repos/"+esc(owner)+"/"+esc(repo)+"/tag_protections/"+itoa(id), p, &out, authToken)
	return out, err
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

// Hook is an org webhook.
type Hook struct {
	ID     int64             `json:"id"`
	Type   string            `json:"type"`
	Active bool              `json:"active"`
	Events []string          `json:"events"`
	Config map[string]string `json:"config"`
}

// NewHook is what CreateOrgHook sends. The secret is the broker's
// GITEA_WEBHOOK_SECRET; Gitea signs the raw body with it.
type NewHook struct {
	URL    string
	Secret string
	Events []string
}

// CreateOrgHook is POST /orgs/{org}/hooks — one hook per group.
func (c *Client) CreateOrgHook(ctx context.Context, org string, h NewHook) (Hook, error) {
	in := struct {
		Type   string            `json:"type"`
		Active bool              `json:"active"`
		Events []string          `json:"events"`
		Config map[string]string `json:"config"`
	}{
		Type:   "gitea",
		Active: true,
		Events: h.Events,
		Config: map[string]string{
			"url":          h.URL,
			"content_type": "json",
			"secret":       h.Secret,
		},
	}
	var out Hook
	err := c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/hooks", in, &out, authToken)
	return out, err
}

// ListOrgHooks is GET /orgs/{org}/hooks.
func (c *Client) ListOrgHooks(ctx context.Context, org string) ([]Hook, error) {
	var all []Hook
	err := paged(func(page int) (int, error) {
		var out []Hook
		if err := c.do(ctx, http.MethodGet, withPage("/orgs/"+esc(org)+"/hooks", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// DeleteOrgHook is DELETE /orgs/{org}/hooks/{id}.
func (c *Client) DeleteOrgHook(ctx context.Context, org string, id int64) error {
	return c.do(ctx, http.MethodDelete, "/orgs/"+esc(org)+"/hooks/"+itoa(id), nil, nil, authToken)
}

// ---------------------------------------------------------------------------
// Commit statuses
// ---------------------------------------------------------------------------

// CommitStatus is a status the broker writes or reads.
type CommitStatus struct {
	ID          int64  `json:"id"`
	Context     string `json:"context"`
	State       string `json:"status"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	// CreatedAt is how old the word is: a deploy status that says "pending"
	// is believed only for so long (deploy.DefaultPatience).
	CreatedAt time.Time `json:"created_at"`
}

// NewStatus is what CreateStatus sends. State is pending, success, error,
// failure, warning or skipped.
type NewStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

// CreateStatus is POST /repos/{o}/{r}/statuses/{sha}.
func (c *Client) CreateStatus(ctx context.Context, owner, repo, sha string, s NewStatus) (CommitStatus, error) {
	var out CommitStatus
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(owner)+"/"+esc(repo)+"/statuses/"+esc(sha), s, &out, authToken)
	return out, err
}

// ListStatuses is GET /repos/{o}/{r}/commits/{ref}/statuses.
func (c *Client) ListStatuses(ctx context.Context, owner, repo, ref string) ([]CommitStatus, error) {
	var all []CommitStatus
	err := paged(func(page int) (int, error) {
		var out []CommitStatus
		if err := c.do(ctx, http.MethodGet, withPage("/repos/"+esc(owner)+"/"+esc(repo)+"/commits/"+esc(ref)+"/statuses", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// Job is one Actions job, as GET /repos/{o}/{r}/actions/jobs/{id} answers it.
// With a job's own token that call is 200 only for the repository the job
// really runs in — 404 for every other, public ones included (ledger
// 2026-09-16). That is what proves a deploy request's repository.
type Job struct {
	ID          int64  `json:"id"`
	RunID       int64  `json:"run_id"`
	Name        string `json:"name"`
	HeadSHA     string `json:"head_sha"`
	HeadBranch  string `json:"head_branch"`
	Status      string `json:"status"`
	RunnerID    int64  `json:"runner_id"`
	WorkflowID  string `json:"workflow_id"`
	DisplayName string `json:"display_title"`
}

// GetJob is GET /repos/{o}/{r}/actions/jobs/{id}, as whatever token this client
// holds.
func (c *Client) GetJob(ctx context.Context, owner, repo, jobID string) (Job, error) {
	var out Job
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/actions/jobs/"+esc(jobID), nil, &out, authToken)
	return out, err
}

// RunnerRegistrationToken is POST /orgs/{org}/actions/runners/registration-token.
// Org scope, never the instance-wide route: labels route jobs, but only the
// registration scope isolates them. GET is a 404 — it is a POST.
func (c *Client) RunnerRegistrationToken(ctx context.Context, org string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/actions/runners/registration-token", nil, &out, authToken)
	return out.Token, err
}

// Run is one workflow run, as GET /orgs/{org}/actions/runs answers it. The two
// repositories are what tells a fork's run from the repository's own, and the
// default branch rides on the repository, so one read says whether a run was
// the reviewed workflow's or a branch's own (D27).
type Run struct {
	ID             int64     `json:"id"`
	Event          string    `json:"event"`
	HeadBranch     string    `json:"head_branch"`
	HeadSHA        string    `json:"head_sha"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at"`
	Repository     Repo      `json:"repository"`
	HeadRepository Repo      `json:"head_repository"`
}

// GetRun is GET /repos/{o}/{r}/actions/runs/{id}, as the broker: what started
// a job, on which branch, from which repository. A job's own token proves
// which run it belongs to (GetJob); the run is read with the broker's.
func (c *Client) GetRun(ctx context.Context, owner, repo string, runID int64) (Run, error) {
	var out Run
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/actions/runs/"+itoa(runID), nil, &out, authToken)
	return out, err
}

// maxRunPages bounds one read of an org's runs: fifty a page, newest first.
const maxRunPages = 20

// ListOrgRunsSince is GET /orgs/{org}/actions/runs, newest first, read until a
// page holds nothing that started at or after since. A run that has not
// started yet carries a zero time and is kept: it is the caller's to ignore.
func (c *Client) ListOrgRunsSince(ctx context.Context, org string, since time.Time) ([]Run, error) {
	var all []Run
	for page := 1; page <= maxRunPages; page++ {
		var out struct {
			Runs []Run `json:"workflow_runs"`
		}
		if err := c.do(ctx, http.MethodGet, withPage("/orgs/"+esc(org)+"/actions/runs", page), nil, &out, authToken); err != nil {
			return nil, err
		}
		recent := false
		for _, run := range out.Runs {
			if run.StartedAt.IsZero() || !run.StartedAt.Before(since) {
				all = append(all, run)
				recent = true
			}
		}
		if len(out.Runs) < pageSize || !recent {
			break
		}
	}
	return all, nil
}

// DispatchWorkflow is POST /repos/{o}/{r}/actions/workflows/{file}/dispatches:
// the workflow file as ref carries it, with the inputs it declares. A workflow
// without a `workflow_dispatch` trigger, or without one of the inputs, is a
// 4xx the caller reports — it is how a repository whose workflow predates D27
// is told apart.
func (c *Client) DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string, inputs map[string]string) error {
	body := struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{Ref: ref, Inputs: inputs}
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/actions/workflows/" + esc(workflow) + "/dispatches"
	return c.do(ctx, http.MethodPost, path, body, nil, authToken)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
