// Package giteatest is an httptest fake of the slice of Gitea the broker
// drives. The mirror and the server drive their tests through it.
//
// It copies two measured behaviours of Gitea 1.27.2 deliberately, because the
// broker depends on both:
//
//   - /users/{login}/tokens answers 401 "auth required" to an API token,
//     however privileged, and takes the site admin's basic auth instead;
//   - GET /user needs read:user; write:repository alone is 403.
package giteatest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// AdminUser and AdminPassword are the fake site admin's basic-auth
// credentials; AdminToken is its API token. They are fixtures, not secrets.
const (
	AdminUser     = "admin"
	AdminPassword = "fake-admin-password"
	AdminToken    = "fake-admin-token"
)

type tokenRow struct {
	gitea.AccessToken
	Owner string
}

// Fake is a running fake Gitea.
type Fake struct {
	mu  sync.Mutex
	srv *httptest.Server

	users   map[string]*gitea.User
	tokens  []tokenRow
	nextID  int64
	orgs    map[string]*gitea.Org
	teams   map[string][]*gitea.Team // org -> teams
	members map[int64]map[string]bool

	repos         map[string]*gitea.Repo       // "org/name"
	collaborators map[string]map[string]string // "org/name" -> login -> permission
	branchRules   map[string][]gitea.BranchProtection
	tagRules      map[string][]gitea.TagProtection
	hooks         map[string][]gitea.Hook // org -> hooks
	statuses      map[string][]gitea.CommitStatus
	jobs          map[string]gitea.Job // "org/repo/jobID"

	// The read side (contents.go): a repository's files at a ref, its branch
	// heads, its tags and the archives of its commits.
	files     map[string]string // "org/repo@ref:path"
	branches  map[string]string // "org/repo@branch" -> sha
	tags      map[string][]gitea.Tag
	annotated map[string]gitea.AnnotatedTag // "org/repo@tagObjectSha"
	archives  map[string][]byte             // "org/repo@sha"

	// Calls is every "METHOD /path" the fake served.
	Calls []string
	// Fail forces a status for one "METHOD /path".
	Fail map[string]int
}

// New starts a fake Gitea with a site admin and closes it when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{
		users:         map[string]*gitea.User{},
		orgs:          map[string]*gitea.Org{},
		teams:         map[string][]*gitea.Team{},
		members:       map[int64]map[string]bool{},
		repos:         map[string]*gitea.Repo{},
		collaborators: map[string]map[string]string{},
		branchRules:   map[string][]gitea.BranchProtection{},
		tagRules:      map[string][]gitea.TagProtection{},
		hooks:         map[string][]gitea.Hook{},
		statuses:      map[string][]gitea.CommitStatus{},
		jobs:          map[string]gitea.Job{},
		files:         map[string]string{},
		branches:      map[string]string{},
		tags:          map[string][]gitea.Tag{},
		annotated:     map[string]gitea.AnnotatedTag{},
		archives:      map[string][]byte{},
		Fail:          map[string]int{},
		nextID:        1,
	}
	f.users[AdminUser] = &gitea.User{ID: f.id(), Login: AdminUser, IsAdmin: true, Active: true}
	f.tokens = append(f.tokens, tokenRow{
		AccessToken: gitea.AccessToken{ID: f.id(), Name: "automation", Scopes: []string{"all"}, Value: AdminToken},
		Owner:       AdminUser,
	})
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *Fake) id() int64 {
	id := f.nextID
	f.nextID++
	return id
}

// URL is the instance origin.
func (f *Fake) URL() string { return f.srv.URL }

// Client builds an admin client with both credentials.
func (f *Fake) Client() *gitea.Client {
	return gitea.New(gitea.Config{
		BaseURL: f.srv.URL, AdminToken: AdminToken,
		AdminUser: AdminUser, AdminPassword: AdminPassword,
		HTTP: f.srv.Client(),
	})
}

// TokenOnly builds a client with an API token and no basic auth — what the
// broker holds when it has no admin password configured.
func (f *Fake) TokenOnly(token string) *gitea.Client {
	return gitea.New(gitea.Config{BaseURL: f.srv.URL, AdminToken: token, HTTP: f.srv.Client()})
}

// AddUser registers a user.
func (f *Fake) AddUser(u gitea.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.ID == 0 {
		u.ID = f.id()
	}
	f.users[u.Login] = &u
}

// AddToken registers a token value for a login with the given scopes.
func (f *Fake) AddToken(login, name, value string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, tokenRow{
		AccessToken: gitea.AccessToken{ID: f.id(), Name: name, Scopes: scopes, Value: value},
		Owner:       login,
	})
}

// AddJob registers an Actions job readable with the given token value.
func (f *Fake) AddJob(owner, repo, jobID string, job gitea.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[owner+"/"+repo+"/"+jobID] = job
}

// User reads back a user.
func (f *Fake) User(login string) (gitea.User, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[login]
	if !ok {
		return gitea.User{}, false
	}
	return *u, true
}

// TeamMembers reads back a team's members, sorted.
func (f *Fake) TeamMembers(org, team string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tm := range f.teams[org] {
		if tm.Name == team {
			var out []string
			for login := range f.members[tm.ID] {
				out = append(out, login)
			}
			slices.Sort(out)
			return out
		}
	}
	return nil
}

// Tokens reads back one login's token names, sorted.
func (f *Fake) Tokens(login string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.tokens {
		if t.Owner == login {
			out = append(out, t.Name)
		}
	}
	slices.Sort(out)
	return out
}

// Repos reads back an org's repository names, sorted.
func (f *Fake) Repos(org string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for full, r := range f.repos {
		if strings.HasPrefix(full, org+"/") {
			out = append(out, r.Name)
		}
	}
	slices.Sort(out)
	return out
}

// BranchRules reads back a repository's branch protections.
func (f *Fake) BranchRules(org, repo string) []gitea.BranchProtection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.branchRules[org+"/"+repo])
}

// TagRules reads back a repository's tag protections.
func (f *Fake) TagRules(org, repo string) []gitea.TagProtection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tagRules[org+"/"+repo])
}

// Hooks reads back an org's hooks.
func (f *Fake) Hooks(org string) []gitea.Hook {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hooks[org])
}

// Wrote reports whether the fake served anything but a GET.
func (f *Fake) Wrote() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if !strings.HasPrefix(c, "GET ") {
			return true
		}
	}
	return false
}

// ResetCalls forgets the call log, so a second pass can be judged on its own.
func (f *Fake) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	key := r.Method + " " + path

	f.mu.Lock()
	f.Calls = append(f.Calls, key)
	forced := f.Fail[key]
	f.mu.Unlock()

	if forced != 0 {
		fail(w, forced, "the test forced this refusal")
		return
	}

	// Token routes: basic auth only, exactly as Gitea 1.27.2 behaves.
	if strings.Contains(path, "/tokens") && strings.HasPrefix(path, "/users/") {
		user, pass, ok := r.BasicAuth()
		if !ok || user != AdminUser || pass != AdminPassword {
			fail(w, http.StatusUnauthorized, "auth required")
			return
		}
		f.tokenRoutes(w, r, path)
		return
	}

	caller, scopes, ok := f.callerOf(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "auth required")
		return
	}

	switch {
	case key == "GET /user":
		if !hasScope(scopes, "read:user") {
			fail(w, http.StatusForbidden, "token does not have at least one of required scope(s), required=[read:user]")
			return
		}
		f.getUser(w, caller)
	case key == "POST /admin/users":
		f.createUser(w, r)
	case r.Method == "GET" && path == "/admin/users":
		f.listUsers(w)
	case r.Method == "PATCH" && strings.HasPrefix(path, "/admin/users/"):
		f.editUser(w, r, seg(path, 3))
	case r.Method == "GET" && strings.HasPrefix(path, "/users/"):
		f.getNamedUser(w, seg(path, 2))
	case key == "POST /orgs":
		f.createOrg(w, r)
	case r.Method == "GET" && path == "/orgs":
		f.listOrgs(w)
	case r.Method == "POST" && strings.HasSuffix(path, "/actions/runners/registration-token"):
		writeJSON(w, 200, map[string]string{"token": "fake-registration-token"})
	case r.Method == "POST" && strings.HasSuffix(path, "/teams"):
		f.createTeam(w, r, seg(path, 2))
	case r.Method == "GET" && strings.HasSuffix(path, "/teams"):
		f.listTeams(w, seg(path, 2))
	case r.Method == "POST" && strings.HasSuffix(path, "/hooks"):
		f.createHook(w, r, seg(path, 2))
	case r.Method == "GET" && strings.HasSuffix(path, "/hooks"):
		writeJSON(w, 200, f.hooksOf(seg(path, 2)))
	case r.Method == "POST" && strings.HasSuffix(path, "/repos"):
		f.createRepo(w, r, seg(path, 2))
	case r.Method == "GET" && strings.HasPrefix(path, "/orgs/") && strings.HasSuffix(path, "/repos"):
		f.listRepos(w, seg(path, 2))
	case strings.HasPrefix(path, "/orgs/") && strings.Contains(path, "/members/"):
		f.orgMember(w, r, seg(path, 2), seg(path, 4))
	case r.Method == "GET" && strings.HasPrefix(path, "/orgs/"):
		f.getOrg(w, seg(path, 2))
	case strings.HasPrefix(path, "/teams/") && strings.Contains(path, "/members"):
		f.teamMember(w, r, path)
	case strings.HasPrefix(path, "/repos/"):
		f.repoRoutes(w, r, path)
	default:
		fail(w, http.StatusNotFound, "the fake does not serve "+key)
	}
}

func (f *Fake) callerOf(r *http.Request) (string, []string, bool) {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.Value == value && value != "" {
			return t.Owner, t.Scopes, true
		}
	}
	return "", nil, false
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == "all" || s == want {
			return true
		}
		// write:x implies read:x, as Gitea's scope model does.
		if strings.HasPrefix(want, "read:") && s == "write:"+strings.TrimPrefix(want, "read:") {
			return true
		}
	}
	return false
}

func (f *Fake) tokenRoutes(w http.ResponseWriter, r *http.Request, path string) {
	login := seg(path, 2)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[login]; !ok {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		row := tokenRow{
			AccessToken: gitea.AccessToken{
				ID: f.id(), Name: in.Name, Scopes: in.Scopes,
				Value: "value-" + login + "-" + in.Name,
			},
			Owner: login,
		}
		row.TokenLastEight = last8(row.Value)
		f.tokens = append(f.tokens, row)
		writeJSON(w, http.StatusCreated, row.AccessToken)
	case http.MethodGet:
		out := []gitea.AccessToken{}
		for _, t := range f.tokens {
			if t.Owner == login {
				redacted := t.AccessToken
				redacted.Value = ""
				out = append(out, redacted)
			}
		}
		writeJSON(w, 200, out)
	case http.MethodDelete:
		// A generation name carries slashes, so the name lives in the escaped
		// path: net/url decodes %2F into a path separator, and real Gitea reads
		// the raw segment.
		name, err := url.PathUnescape(seg(r.URL.EscapedPath(), 6))
		if err != nil {
			fail(w, http.StatusBadRequest, "bad token name")
			return
		}
		for i, t := range f.tokens {
			if t.Owner == login && (t.Name == name || strconv.FormatInt(t.ID, 10) == name) {
				f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		fail(w, http.StatusNotFound, "no such token")
	default:
		fail(w, http.StatusMethodNotAllowed, "no")
	}
}

func (f *Fake) getUser(w http.ResponseWriter, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[login]
	if !ok {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	writeJSON(w, 200, u)
}

func (f *Fake) getNamedUser(w http.ResponseWriter, login string) { f.getUser(w, login) }

func (f *Fake) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username   string `json:"username"`
		Email      string `json:"email"`
		FullName   string `json:"full_name"`
		Password   string `json:"password"`
		Restricted bool   `json:"restricted"`
		Visibility string `json:"visibility"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.users[in.Username]; exists {
		fail(w, http.StatusUnprocessableEntity, "user already exists")
		return
	}
	if in.Password == "" {
		fail(w, http.StatusUnprocessableEntity, "a password is required")
		return
	}
	u := &gitea.User{
		ID: f.id(), Login: in.Username, Email: in.Email, FullName: in.FullName,
		Restricted: in.Restricted, Active: true, Visibility: in.Visibility,
	}
	f.users[in.Username] = u
	writeJSON(w, http.StatusCreated, u)
}

func (f *Fake) listUsers(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gitea.User{}
	for _, u := range f.users {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b gitea.User) int { return strings.Compare(a.Login, b.Login) })
	writeJSON(w, 200, out)
}

func (f *Fake) editUser(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		Active     *bool `json:"active"`
		Restricted *bool `json:"restricted"`
		Admin      *bool `json:"admin"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[login]
	if !ok {
		fail(w, http.StatusNotFound, "no such user")
		return
	}
	if in.Active != nil {
		u.Active = *in.Active
	}
	if in.Restricted != nil {
		u.Restricted = *in.Restricted
	}
	if in.Admin != nil {
		u.IsAdmin = *in.Admin
	}
	writeJSON(w, 200, u)
}

func (f *Fake) createOrg(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username   string `json:"username"`
		FullName   string `json:"full_name"`
		Visibility string `json:"visibility"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.orgs[in.Username]; exists {
		fail(w, http.StatusUnprocessableEntity, "org already exists")
		return
	}
	o := &gitea.Org{ID: f.id(), Name: in.Username, FullName: in.FullName, Visibility: in.Visibility}
	f.orgs[in.Username] = o
	writeJSON(w, http.StatusCreated, o)
}

func (f *Fake) getOrg(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orgs[name]
	if !ok {
		fail(w, http.StatusNotFound, "no such org")
		return
	}
	writeJSON(w, 200, o)
}

func (f *Fake) listOrgs(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gitea.Org{}
	for _, o := range f.orgs {
		out = append(out, *o)
	}
	slices.SortFunc(out, func(a, b gitea.Org) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, 200, out)
}

func (f *Fake) createTeam(w http.ResponseWriter, r *http.Request, org string) {
	var in struct {
		Name       string            `json:"name"`
		Permission string            `json:"permission"`
		UnitsMap   map[string]string `json:"units_map"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orgs[org]; !ok {
		fail(w, http.StatusNotFound, "no such org")
		return
	}
	for _, t := range f.teams[org] {
		if t.Name == in.Name {
			fail(w, http.StatusUnprocessableEntity, "team already exists")
			return
		}
	}
	t := &gitea.Team{ID: f.id(), Name: in.Name, Permission: in.Permission, IncludesAllRepositories: true, UnitsMap: in.UnitsMap}
	f.teams[org] = append(f.teams[org], t)
	writeJSON(w, http.StatusCreated, t)
}

func (f *Fake) listTeams(w http.ResponseWriter, org string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gitea.Team{}
	for _, t := range f.teams[org] {
		out = append(out, *t)
	}
	writeJSON(w, 200, out)
}

func (f *Fake) teamMember(w http.ResponseWriter, r *http.Request, path string) {
	id, _ := strconv.ParseInt(seg(path, 2), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members[id] == nil {
		f.members[id] = map[string]bool{}
	}
	switch r.Method {
	case http.MethodPut:
		login := seg(path, 4)
		if _, ok := f.users[login]; !ok {
			fail(w, http.StatusNotFound, "no such user")
			return
		}
		f.members[id][login] = true
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		delete(f.members[id], seg(path, 4))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		out := []gitea.User{}
		for login := range f.members[id] {
			if u, ok := f.users[login]; ok {
				out = append(out, *u)
			}
		}
		slices.SortFunc(out, func(a, b gitea.User) int { return strings.Compare(a.Login, b.Login) })
		writeJSON(w, 200, out)
	default:
		fail(w, http.StatusMethodNotAllowed, "no")
	}
}

func (f *Fake) orgMember(w http.ResponseWriter, r *http.Request, org, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.teams[org] {
		if f.members[t.ID][login] {
			if r.Method == http.MethodDelete {
				for _, t2 := range f.teams[org] {
					delete(f.members[t2.ID], login)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	fail(w, http.StatusNotFound, "not a member")
}

func (f *Fake) createHook(w http.ResponseWriter, r *http.Request, org string) {
	var in struct {
		Events []string          `json:"events"`
		Config map[string]string `json:"config"`
		Type   string            `json:"type"`
		Active bool              `json:"active"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	h := gitea.Hook{ID: f.id(), Type: in.Type, Active: in.Active, Events: in.Events, Config: in.Config}
	f.hooks[org] = append(f.hooks[org], h)
	writeJSON(w, http.StatusCreated, h)
}

func (f *Fake) hooksOf(org string) []gitea.Hook {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.hooks[org])
	if out == nil {
		out = []gitea.Hook{}
	}
	// Gitea never returns a hook's secret.
	for i := range out {
		cfg := map[string]string{}
		for k, v := range out[i].Config {
			if k != "secret" {
				cfg[k] = v
			}
		}
		out[i].Config = cfg
	}
	return out
}

func (f *Fake) createRepo(w http.ResponseWriter, r *http.Request, org string) {
	var in struct {
		Name          string `json:"name"`
		Private       bool   `json:"private"`
		AutoInit      bool   `json:"auto_init"`
		DefaultBranch string `json:"default_branch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.orgs[org]; !ok {
		fail(w, http.StatusNotFound, "no such org")
		return
	}
	full := org + "/" + in.Name
	if _, exists := f.repos[full]; exists {
		fail(w, http.StatusConflict, "repository already exists")
		return
	}
	repo := &gitea.Repo{
		ID: f.id(), Name: in.Name, FullName: full, Private: in.Private,
		Empty: !in.AutoInit, DefaultBranch: in.DefaultBranch,
		CloneURL: f.srv.URL + "/" + full + ".git",
		HTMLURL:  f.srv.URL + "/" + full,
	}
	f.repos[full] = repo
	writeJSON(w, http.StatusCreated, repo)
}

func (f *Fake) listRepos(w http.ResponseWriter, org string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []gitea.Repo{}
	for full, r := range f.repos {
		if strings.HasPrefix(full, org+"/") {
			out = append(out, *r)
		}
	}
	slices.SortFunc(out, func(a, b gitea.Repo) int { return strings.Compare(a.FullName, b.FullName) })
	writeJSON(w, 200, out)
}

func (f *Fake) repoRoutes(w http.ResponseWriter, r *http.Request, path string) {
	owner, repo := seg(path, 2), seg(path, 3)
	full := owner + "/" + repo
	rest := strings.TrimPrefix(path, "/repos/"+owner+"/"+repo)

	f.mu.Lock()
	_, known := f.repos[full]
	f.mu.Unlock()
	if !known {
		fail(w, http.StatusNotFound, "no such repository")
		return
	}

	switch {
	case rest == "":
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, 200, f.repos[full])
	case strings.HasPrefix(rest, "/collaborators/"):
		f.collaborator(w, r, full, strings.TrimPrefix(rest, "/collaborators/"))
	case strings.HasPrefix(rest, "/branch_protections"):
		f.branchProtection(w, r, full, strings.TrimPrefix(rest, "/branch_protections"))
	case strings.HasPrefix(rest, "/tag_protections"):
		f.tagProtection(w, r, full, strings.TrimPrefix(rest, "/tag_protections"))
	case strings.HasPrefix(rest, "/statuses/"):
		f.status(w, r, full, strings.TrimPrefix(rest, "/statuses/"))
	case strings.HasPrefix(rest, "/commits/") && strings.HasSuffix(rest, "/statuses"):
		ref := strings.TrimSuffix(strings.TrimPrefix(rest, "/commits/"), "/statuses")
		f.mu.Lock()
		defer f.mu.Unlock()
		out := f.statuses[full+"@"+ref]
		if out == nil {
			out = []gitea.CommitStatus{}
		}
		writeJSON(w, 200, out)
	case f.serveContents(w, r, full, rest):
	case strings.HasPrefix(rest, "/actions/jobs/"):
		f.mu.Lock()
		defer f.mu.Unlock()
		job, ok := f.jobs[full+"/"+strings.TrimPrefix(rest, "/actions/jobs/")]
		if !ok {
			fail(w, http.StatusNotFound, "no such job")
			return
		}
		writeJSON(w, 200, job)
	default:
		fail(w, http.StatusNotFound, "the fake does not serve "+path)
	}
}

func (f *Fake) collaborator(w http.ResponseWriter, r *http.Request, full, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.collaborators[full] == nil {
		f.collaborators[full] = map[string]string{}
	}
	switch r.Method {
	case http.MethodPut:
		var in struct {
			Permission string `json:"permission"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.collaborators[full][login] = in.Permission
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		if _, ok := f.collaborators[full][login]; ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fail(w, http.StatusNotFound, "not a collaborator")
	case http.MethodDelete:
		delete(f.collaborators[full], login)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *Fake) branchProtection(w http.ResponseWriter, r *http.Request, full, rest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && rest == "":
		out := f.branchRules[full]
		if out == nil {
			out = []gitea.BranchProtection{}
		}
		writeJSON(w, 200, out)
	case r.Method == http.MethodPost:
		var p gitea.BranchProtection
		_ = json.NewDecoder(r.Body).Decode(&p)
		for _, existing := range f.branchRules[full] {
			if existing.RuleName == p.RuleName {
				fail(w, http.StatusUnprocessableEntity, "rule already exists")
				return
			}
		}
		f.branchRules[full] = append(f.branchRules[full], p)
		writeJSON(w, http.StatusCreated, p)
	case r.Method == http.MethodPatch:
		name := strings.TrimPrefix(rest, "/")
		var p gitea.BranchProtection
		_ = json.NewDecoder(r.Body).Decode(&p)
		for i, existing := range f.branchRules[full] {
			if existing.RuleName == name {
				p.RuleName = name
				f.branchRules[full][i] = p
				writeJSON(w, 200, p)
				return
			}
		}
		fail(w, http.StatusNotFound, "no such rule")
	default:
		fail(w, http.StatusNotFound, "no")
	}
}

func (f *Fake) tagProtection(w http.ResponseWriter, r *http.Request, full, rest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && rest == "":
		out := f.tagRules[full]
		if out == nil {
			out = []gitea.TagProtection{}
		}
		writeJSON(w, 200, out)
	case r.Method == http.MethodPost:
		var p gitea.TagProtection
		_ = json.NewDecoder(r.Body).Decode(&p)
		p.ID = f.id()
		f.tagRules[full] = append(f.tagRules[full], p)
		writeJSON(w, http.StatusCreated, p)
	case r.Method == http.MethodPatch:
		id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "/"), 10, 64)
		var p gitea.TagProtection
		_ = json.NewDecoder(r.Body).Decode(&p)
		for i, existing := range f.tagRules[full] {
			if existing.ID == id {
				p.ID = id
				f.tagRules[full][i] = p
				writeJSON(w, 200, p)
				return
			}
		}
		fail(w, http.StatusNotFound, "no such rule")
	default:
		fail(w, http.StatusNotFound, "no")
	}
}

func (f *Fake) status(w http.ResponseWriter, r *http.Request, full, sha string) {
	var in gitea.NewStatus
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := gitea.CommitStatus{ID: f.id(), Context: in.Context, State: in.State, Description: in.Description, TargetURL: in.TargetURL}
	f.statuses[full+"@"+sha] = append(f.statuses[full+"@"+sha], s)
	writeJSON(w, http.StatusCreated, s)
}

// seg returns the n-th path segment, counting the empty one before the leading
// slash as 0: seg("/orgs/acme/teams", 2) == "acme".
func seg(path string, n int) string {
	parts := strings.Split(path, "/")
	if n < len(parts) {
		return parts[n]
	}
	return ""
}

func last8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}
