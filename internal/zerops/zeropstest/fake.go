// Package zeropstest is an httptest fake of the slice of the Zerops API the
// broker uses. Every package that talks to Zerops drives its tests through it.
//
// It is deliberately literal about identity: a bearer value maps to an
// identity, /user/info answers that identity's own id, and a token can only
// read the integration tokens of its own org — the three properties the
// throwaway check leans on.
package zeropstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// Identity is who a bearer value is. UserInfoID is what /user/info answers as
// `id`; for an integration token the platform answers the token's own id, so a
// fake whose UserInfoID differs from its TokenID is a token that is not what it
// claims to be.
type Identity struct {
	UserInfoID string
	TokenID    string
	ClientID   string
	Email      string
	FullName   string
}

// Fake is a running fake API.
type Fake struct {
	mu  sync.Mutex
	srv *httptest.Server

	// Now is the API's clock: what the Date header says. Tests move it.
	Now time.Time
	// ClientID is the org this API belongs to.
	ClientID string

	identities map[string]Identity         // bearer value -> identity
	tokens     map[string]zerops.Token     // token id -> metadata
	members    []zerops.Member             // the org's member list
	projects   []zerops.Project            // the org's projects
	services   map[string][]zerops.Service // project id -> services

	// Fail forces a status for one "METHOD /path" (the path after
	// /api/rest/public). Used to prove the fail-safe rules.
	Fail map[string]int
	// TruncateProjectSearch makes the search claim a higher totalHits than it
	// returns — a page short of its declared total.
	TruncateProjectSearch bool

	// Requests logs every call, so a test can assert that a refused pass wrote
	// nothing.
	Requests []string
	// Stopped tracks PUT /service-stack/{id}/stop|start.
	Stopped map[string]bool
	started map[string]bool
	// Imports collects the yaml of every service-stack import.
	Imports []Import
	// Minted collects every token minted, by name.
	Minted []zerops.TokenSpec
	// Deleted collects every token id deleted.
	Deleted []string
}

// Import is one recorded service-stack import.
type Import struct {
	ProjectID string
	Yaml      string
}

// New starts a fake and stops it when the test ends.
func New(t *testing.T, clientID string) *Fake {
	t.Helper()
	f := &Fake{
		Now:        time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		ClientID:   clientID,
		identities: map[string]Identity{},
		tokens:     map[string]zerops.Token{},
		services:   map[string][]zerops.Service{},
		Fail:       map[string]int{},
		Stopped:    map[string]bool{},
		started:    map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the API base a client is built with.
func (f *Fake) URL() string { return f.srv.URL }

// Client builds a zerops client authenticated as the given bearer value.
func (f *Fake) Client(bearer string) *zerops.Client {
	return zerops.New(f.srv.URL, bearer, f.srv.Client())
}

// AddIdentity registers a bearer value.
func (f *Fake) AddIdentity(bearer string, id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identities[bearer] = id
}

// AddToken registers an integration token's metadata under its id.
func (f *Fake) AddToken(tok zerops.Token) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[tok.ID] = tok
}

// AddMember appends a row to the org's member list.
func (f *Fake) AddMember(m zerops.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ClientID == "" {
		m.ClientID = f.ClientID
	}
	f.members = append(f.members, m)
}

// ClearMembers empties the member list — what a read that comes back with
// nothing looks like.
func (f *Fake) ClearMembers() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = nil
}

// SetProjects replaces the org's projects.
func (f *Fake) SetProjects(p ...zerops.Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = p
}

// SetServices replaces one project's services.
func (f *Fake) SetServices(projectID string, s ...zerops.Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[projectID] = s
}

// Project returns the recorded project, so a test can read back a tag write.
func (f *Fake) Project(id string) (zerops.Project, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.projects {
		if p.ID == id {
			return p, true
		}
	}
	return zerops.Project{}, false
}

// Wrote reports whether any request other than a read was made.
func (f *Fake) Wrote() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Requests {
		switch {
		case strings.HasPrefix(r, "GET "):
		case r == "POST /project/search", r == "POST /service-stack/search":
		default:
			return true
		}
	}
	return false
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/rest/public")
	key := r.Method + " " + path

	f.mu.Lock()
	f.Requests = append(f.Requests, key)
	w.Header().Set("Date", f.Now.Format(http.TimeFormat))
	forced := f.Fail[key]
	identity, known := f.identities[bearer(r)]
	f.mu.Unlock()

	if forced != 0 {
		writeErr(w, forced, "forced", "the test forced this refusal")
		return
	}
	if !known {
		writeErr(w, http.StatusUnauthorized, "invalidAccessToken", "unknown token")
		return
	}

	switch {
	case key == "GET /user/info":
		f.userInfo(w, identity)
	case r.Method == "GET" && strings.HasSuffix(path, "/integration-token/list"):
		f.tokenList(w, path, identity)
	case r.Method == "GET" && strings.Contains(path, "/integration-token/"):
		f.token(w, path, identity)
	case r.Method == "POST" && strings.HasSuffix(path, "/integration-token"):
		f.mintToken(w, r, path, identity)
	case r.Method == "PUT" && strings.Contains(path, "/integration-token/"):
		f.updateToken(w, r, path, identity)
	case r.Method == "DELETE" && strings.Contains(path, "/integration-token/"):
		f.deleteToken(w, path, identity)
	case r.Method == "GET" && strings.HasSuffix(path, "/user/list"):
		f.memberList(w, path, identity)
	case key == "POST /project/search":
		f.projectSearch(w, r)
	case r.Method == "GET" && strings.HasPrefix(path, "/project/"):
		f.project(w, path)
	case r.Method == "PUT" && strings.HasPrefix(path, "/project/"):
		f.updateProject(w, r, path)
	case key == "POST /service-stack/search":
		f.serviceSearch(w, r)
	case r.Method == "POST" && strings.HasSuffix(path, "/service-stack/import"):
		f.importServices(w, r, path)
	case r.Method == "PUT" && (strings.HasSuffix(path, "/stop") || strings.HasSuffix(path, "/start")):
		f.stopStart(w, path)
	default:
		writeErr(w, http.StatusNotFound, "notFound", "the fake does not serve "+key)
	}
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (f *Fake) userInfo(w http.ResponseWriter, id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := f.tokens[id.TokenID]
	writeJSON(w, 200, zerops.UserInfo{
		ID:       id.UserInfoID,
		Email:    id.Email,
		FullName: id.FullName,
		Clients: []zerops.Membership{{
			ID:                id.TokenID,
			ClientID:          id.ClientID,
			RoleCode:          tok.RoleCode,
			Status:            "ACTIVE",
			CanCreateProjects: tok.CanCreateProjects,
			CanViewFinances:   tok.CanViewFinances,
			CanEditFinances:   tok.CanEditFinances,
		}},
	})
}

// orgOf pulls {org} out of /client/{org}/…
func orgOf(path string) string {
	rest := strings.TrimPrefix(path, "/client/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func lastSegment(path string) string {
	i := strings.LastIndex(path, "/")
	return path[i+1:]
}

func (f *Fake) token(w http.ResponseWriter, path string, id Identity) {
	// A token from another org cannot read this org's tokens (the wrong_org
	// step of the throwaway check).
	if orgOf(path) != id.ClientID {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "not your org")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[lastSegment(path)]
	if !ok {
		writeErr(w, http.StatusNotFound, "notFound", "no such token")
		return
	}
	writeJSON(w, 200, tok)
}

func (f *Fake) tokenList(w http.ResponseWriter, path string, id Identity) {
	if orgOf(path) != id.ClientID {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "not your org")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]zerops.Token, 0, len(f.tokens))
	for _, t := range f.tokens {
		list = append(list, t)
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

func (f *Fake) mintToken(w http.ResponseWriter, r *http.Request, path string, id Identity) {
	if orgOf(path) != id.ClientID {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "not your org")
		return
	}
	var spec zerops.TokenSpec
	_ = json.NewDecoder(r.Body).Decode(&spec)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Minted = append(f.Minted, spec)
	tok := zerops.Token{
		ID: "tok-" + spec.Name, Name: spec.Name, RoleCode: spec.RoleCode,
		CreatedByUser: id.UserInfoID, Created: f.Now, Projects: spec.Projects,
	}
	f.tokens[tok.ID] = tok
	writeJSON(w, 200, zerops.RawToken{Token: tok, Value: "value-" + tok.ID})
}

func (f *Fake) updateToken(w http.ResponseWriter, r *http.Request, path string, id Identity) {
	if orgOf(path) != id.ClientID {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "not your org")
		return
	}
	var spec zerops.TokenSpec
	_ = json.NewDecoder(r.Body).Decode(&spec)
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, ok := f.tokens[lastSegment(path)]
	if !ok {
		writeErr(w, http.StatusNotFound, "notFound", "no such token")
		return
	}
	tok.Name, tok.RoleCode, tok.Projects = spec.Name, spec.RoleCode, spec.Projects
	f.tokens[tok.ID] = tok
	writeJSON(w, 200, tok)
}

func (f *Fake) deleteToken(w http.ResponseWriter, path string, id Identity) {
	if orgOf(path) != id.ClientID {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "not your org")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tokenID := lastSegment(path)
	if _, ok := f.tokens[tokenID]; !ok {
		writeErr(w, http.StatusNotFound, "notFound", "no such token")
		return
	}
	delete(f.tokens, tokenID)
	f.Deleted = append(f.Deleted, tokenID)
	w.WriteHeader(http.StatusOK)
}

func (f *Fake) memberList(w http.ResponseWriter, path string, id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, 200, map[string]any{"clientUserList": f.members})
}

func (f *Fake) projectSearch(w http.ResponseWriter, r *http.Request) {
	var filter struct {
		Search []struct{ Name, Operator, Value string } `json:"search"`
		Limit  int                                      `json:"limit"`
		Offset int                                      `json:"offset"`
	}
	_ = json.NewDecoder(r.Body).Decode(&filter)

	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.projects
	for _, term := range filter.Search {
		if term.Name == "id" {
			var kept []zerops.Project
			for _, p := range items {
				if p.ID == term.Value {
					kept = append(kept, p)
				}
			}
			items = kept
		}
	}
	total := len(items)
	if f.TruncateProjectSearch {
		total = len(items) + 1
	}
	writeJSON(w, 200, map[string]any{"items": items, "totalHits": total, "limit": filter.Limit, "offset": filter.Offset})
}

func (f *Fake) project(w http.ResponseWriter, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := lastSegment(path)
	for _, p := range f.projects {
		if p.ID == id {
			writeJSON(w, 200, p)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "projectNotFound", "no such project")
}

func (f *Fake) updateProject(w http.ResponseWriter, r *http.Request, path string) {
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	if _, ok := body["userRoles"]; ok {
		writeErr(w, http.StatusBadRequest, "invalidUserInput", "userRoles must not be sent")
		return
	}
	var update zerops.ProjectUpdate
	raw, _ := json.Marshal(body)
	_ = json.Unmarshal(raw, &update)

	f.mu.Lock()
	defer f.mu.Unlock()
	id := lastSegment(path)
	for i, p := range f.projects {
		if p.ID == id {
			f.projects[i].Name = update.Name
			f.projects[i].Description = update.Description
			f.projects[i].TagList = update.TagList
			writeJSON(w, 200, f.projects[i])
			return
		}
	}
	writeErr(w, http.StatusNotFound, "projectNotFound", "no such project")
}

func (f *Fake) serviceSearch(w http.ResponseWriter, r *http.Request) {
	var filter struct {
		Search []struct{ Name, Operator, Value string } `json:"search"`
	}
	_ = json.NewDecoder(r.Body).Decode(&filter)
	projectID := ""
	for _, term := range filter.Search {
		if term.Name == "projectId" {
			projectID = term.Value
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.services[projectID]
	if items == nil {
		items = []zerops.Service{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "totalHits": len(items)})
}

func (f *Fake) importServices(w http.ResponseWriter, r *http.Request, path string) {
	var body struct {
		Yaml string `json:"yaml"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	projectID := strings.TrimSuffix(strings.TrimPrefix(path, "/project/"), "/service-stack/import")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Imports = append(f.Imports, Import{ProjectID: projectID, Yaml: body.Yaml})
	writeJSON(w, 200, map[string]any{"serviceStacks": []any{}})
}

func (f *Fake) stopStart(w http.ResponseWriter, path string) {
	parts := strings.Split(strings.TrimPrefix(path, "/service-stack/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	stop := parts[1] == "stop"
	f.Stopped[parts[0]] = stop
	if !stop {
		f.started[parts[0]] = true
	}
	for i, s := range f.services[f.projectOfService(parts[0])] {
		if s.ID == parts[0] {
			status := "ACTIVE"
			if stop {
				status = "STOPPED"
			}
			f.services[f.projectOfService(parts[0])][i].Status = status
		}
	}
	writeJSON(w, 200, map[string]any{"id": "proc-1"})
}

// projectOfService finds which project a service belongs to. Called with the
// lock held.
func (f *Fake) projectOfService(serviceID string) string {
	for projectID, list := range f.services {
		for _, s := range list {
			if s.ID == serviceID {
				return projectID
			}
		}
	}
	return ""
}

// Started reports whether PUT /service-stack/{id}/start was ever called.
func (f *Fake) Started(serviceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[serviceID]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
