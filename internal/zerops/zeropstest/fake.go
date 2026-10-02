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
	"strconv"
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
	// FailTimes bounds a Fail entry: it answers that many times, then the
	// route serves again — what an intermittent refusal looks like.
	FailTimes map[string]int
	// TruncateProjectSearch makes the search claim a higher totalHits than it
	// returns — a page short of its declared total.
	TruncateProjectSearch bool

	// Requests logs every call, so a test can assert that a refused pass wrote
	// nothing.
	Requests []string
	// stopped and started track PUT /service-stack/{id}/stop|start. They are
	// read through IsStopped and Started, under the lock: the fake serves
	// requests on its own goroutines, so a test that indexed the map directly
	// would race the handler that writes it.
	stopped map[string]bool
	started map[string]bool
	// Imports collects the yaml of every service-stack import.
	Imports []Import
	// Minted collects every token minted, by name.
	Minted []zerops.TokenSpec
	// Deleted collects every token id deleted.
	Deleted []string
	// DeletedServices collects every service id deleted.
	DeletedServices []string
	// FailDeletes makes the process a deletion answers end FAILED — what a
	// service the platform would not remove looks like.
	FailDeletes bool
	// FailImports makes a service-stack import a 400 whose message quotes the
	// document — what a refusal that echoes its input looks like.
	FailImports bool
	// HoldDeletes, when set, keeps every service deletion waiting until it is
	// closed — a deletion still in flight.
	HoldDeletes chan struct{}
	// importBuild, when set, makes an import do what the platform's does: each
	// service in the document appears READY_TO_DEPLOY, made now, and the import
	// answers its stack.create and stack.build processes, the build ending
	// with this status. Unset, an import is only recorded.
	importBuild string
	// Ungranted is the projects this API's tokens were never granted: a
	// service search on one, and the user data of its services, answer 403 —
	// what a Mate project the app has not yet granted the broker looks like.
	Ungranted map[string]bool
	// userData is each service's own variables, by service id.
	userData map[string][]zerops.ServiceUserData
	// searchIndex, when set, is what POST /project/search answers in place
	// of the projects GET /project/{id} reads: an index that lags.
	searchIndex []zerops.Project
	// hidden is, by service id, the variables its next list reads leave out
	// though they exist: a create of one is still refused as a duplicate.
	hidden map[string]hiddenUserData

	// The deploy half (deploy.go): app versions by id, processes by id, which
	// services have ever deployed, whose subdomain is on, and which version
	// names are doomed to a failed build.
	versions   map[string]*AppVersionRecord
	processes  map[string]zerops.Process
	deployed   map[string]bool
	subdomains map[string]bool
	doomed     map[string]bool
	// named is the version each service's userData names, by service id.
	named    map[string]string
	sequence int
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
		FailTimes:  map[string]int{},
		Ungranted:  map[string]bool{},
		userData:   map[string][]zerops.ServiceUserData{},
		hidden:     map[string]hiddenUserData{},
		stopped:    map[string]bool{},
		started:    map[string]bool{},
		versions:   map[string]*AppVersionRecord{},
		processes:  map[string]zerops.Process{},
		deployed:   map[string]bool{},
		subdomains: map[string]bool{},
		doomed:     map[string]bool{},
		named:      map[string]string{},
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

// FreezeSearch makes POST /project/search answer the projects as they are
// now while later SetProjects calls reach GET /project/{id} alone: the
// platform's search index lagging its own reads (0.5–2.6 s, measured).
// ThawSearch catches it up.
func (f *Fake) FreezeSearch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchIndex = append([]zerops.Project{}, f.projects...)
}

// ThawSearch makes the search answer what GET answers again.
func (f *Fake) ThawSearch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchIndex = nil
}

// SetServices replaces one project's services.
func (f *Fake) SetServices(projectID string, s ...zerops.Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[projectID] = s
}

// SetUserData replaces one service's own variables. An entry without an id
// gets one.
func (f *Fake) SetUserData(serviceID string, entries ...zerops.ServiceUserData) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]zerops.ServiceUserData, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			f.sequence++
			e.ID = serviceID + "-ud-" + itoa(f.sequence)
		}
		list = append(list, e)
	}
	f.userData[serviceID] = list
}

// hiddenUserData is a list that leaves keys out for a number of reads.
type hiddenUserData struct {
	reads int
	keys  map[string]bool
}

// HideUserData makes the next reads list reads of a service's variables leave
// keys out, though the variables exist and a create of one is refused as a
// duplicate: what a read the platform answers short looks like to the broker
// (the paged list of 2026-09-30, or a variable written a moment ago and not
// listed yet).
func (f *Fake) HideUserData(serviceID string, reads int, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := hiddenUserData{reads: reads, keys: map[string]bool{}}
	for _, k := range keys {
		h.keys[k] = true
	}
	f.hidden[serviceID] = h
}

// UserData reads back one service's own variables, in the order they were
// written.
func (f *Fake) UserData(serviceID string) []zerops.ServiceUserData {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zerops.ServiceUserData(nil), f.userData[serviceID]...)
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

// Served counts the requests made to one "METHOD /path".
func (f *Fake) Served(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if r == key {
			n++
		}
	}
	return n
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
	// A pre-signed blob URL carries no token and is outside the API prefix.
	if strings.HasPrefix(r.URL.Path, appCodePath) {
		f.appCodeBlob(w, r.URL.Path)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/rest/public")
	key := r.Method + " " + path

	f.mu.Lock()
	f.Requests = append(f.Requests, key)
	w.Header().Set("Date", f.Now.Format(http.TimeFormat))
	forced := f.Fail[key]
	if n, bounded := f.FailTimes[key]; forced != 0 && bounded {
		if n <= 1 {
			delete(f.Fail, key)
			delete(f.FailTimes, key)
		} else {
			f.FailTimes[key] = n - 1
		}
	}
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
	case r.Method == "GET" && strings.HasPrefix(path, "/project/") && strings.HasSuffix(path, "/process"):
		f.projectProcesses(w, strings.TrimSuffix(strings.TrimPrefix(path, "/project/"), "/process"))
	case r.Method == "GET" && strings.HasPrefix(path, "/project/"):
		f.project(w, path)
	case r.Method == "PUT" && strings.HasPrefix(path, "/project/"):
		f.updateProject(w, r, path)
	case key == "POST /service-stack/search":
		f.serviceSearch(w, r)
	case r.Method == "GET" && strings.HasPrefix(path, "/service-stack/") && strings.HasSuffix(path, "/user-data"):
		f.listUserData(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/service-stack/"), "/user-data"))
	case r.Method == "POST" && strings.HasPrefix(path, "/service-stack/") && strings.HasSuffix(path, "/user-data"):
		f.createUserData(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/service-stack/"), "/user-data"))
	case r.Method == "PUT" && strings.HasPrefix(path, "/user-data/"):
		f.updateUserData(w, r, lastSegment(path))
	case r.Method == "DELETE" && strings.HasPrefix(path, "/user-data/"):
		f.deleteUserData(w, lastSegment(path))
	case r.Method == "POST" && strings.HasSuffix(path, "/service-stack/import"):
		f.importServices(w, r, path)
	case r.Method == "PUT" && (strings.HasSuffix(path, "/stop") || strings.HasSuffix(path, "/start")):
		f.stopStart(w, path)
	case f.serveDeploy(w, r, path, key):
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
	if f.searchIndex != nil {
		items = f.searchIndex
	}
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

// project is GET /project/{id}. A project that is not there answers 400
// projectNotFound, never 404 — what the platform answers for a deleted project
// (measured 2026-09-16 and 2026-09-20).
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
	writeErr(w, http.StatusBadRequest, "projectNotFound", "no such project")
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
	if f.Ungranted[projectID] {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "the token does not hold that project")
		return
	}
	items := f.services[projectID]
	if items == nil {
		items = []zerops.Service{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "totalHits": len(items)})
}

// listUserData is GET /service-stack/{id}/user-data. Sensitive values come
// back in clear, as they do to a BASIC_USER token on the project. It pages as
// the platform does: 20 a page unless ?limit= says otherwise, from ?offset=,
// with the whole count in total (measured 2026-09-30: a zcp container holds
// 29, its system variables among them).
func (f *Fake) listUserData(w http.ResponseWriter, r *http.Request, serviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.userDataReachable(w, serviceID) {
		return
	}
	all := f.userData[serviceID]
	if h := f.hidden[serviceID]; h.reads > 0 {
		h.reads--
		f.hidden[serviceID] = h
		var listed []zerops.ServiceUserData
		for _, e := range all {
			if !h.keys[e.Key] {
				listed = append(listed, e)
			}
		}
		all = listed
	}
	limit, offset := 20, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	list := []zerops.ServiceUserData{}
	if offset < len(all) {
		list = all[offset:min(offset+limit, len(all))]
	}
	writeJSON(w, 200, map[string]any{"list": list, "count": len(list), "total": len(all), "limit": limit, "offset": offset})
}

// createUserData is POST /service-stack/{id}/user-data: 200 with a process. A
// write onto a service that has not deployed yet is accepted, as measured. A
// key the service holds already, in any case, is refused as the platform
// refuses it (400 userDataDuplicateKey, measured 2026-09-30).
func (f *Fake) createUserData(w http.ResponseWriter, r *http.Request, serviceID string) {
	var spec zerops.UserDataSpec
	_ = json.NewDecoder(r.Body).Decode(&spec)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.userDataReachable(w, serviceID) {
		return
	}
	if spec.Key == "" {
		writeErr(w, http.StatusBadRequest, "invalidUserInput", "a variable has a key")
		return
	}
	for _, e := range f.userData[serviceID] {
		if strings.EqualFold(e.Key, spec.Key) {
			writeErr(w, http.StatusBadRequest, "userDataDuplicateKey",
				"Service environment variable key '"+spec.Key+"' is not unique (case insensitive).")
			return
		}
	}
	f.sequence++
	f.userData[serviceID] = append(f.userData[serviceID], zerops.ServiceUserData{
		ID: serviceID + "-ud-" + itoa(f.sequence), Key: spec.Key, Content: spec.Content, Sensitive: spec.Sensitive,
	})
	f.sequence++
	proc := zerops.Process{ID: "proc-" + itoa(f.sequence), ServiceStackID: serviceID, Status: zerops.ProcessFinished, ActionName: "stack.userData.create"}
	f.processes[proc.ID] = proc
	writeJSON(w, 200, proc)
}

// updateUserData is PUT /user-data/{id}. The body must carry the key beside
// the content; the platform refuses one without it. Whether the platform
// renames a variable whose stored key differs in case is not measured, so the
// fake keeps the stored key: nothing may rely on a PUT to rename.
func (f *Fake) updateUserData(w http.ResponseWriter, r *http.Request, id string) {
	var spec zerops.UserDataSpec
	_ = json.NewDecoder(r.Body).Decode(&spec)
	if spec.Key == "" {
		writeErr(w, http.StatusBadRequest, "invalidUserInput", "the key must be sent beside the content")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for serviceID, list := range f.userData {
		for i, e := range list {
			if e.ID != id {
				continue
			}
			if !f.userDataReachable(w, serviceID) {
				return
			}
			f.userData[serviceID][i].Content = spec.Content
			writeJSON(w, 200, f.userData[serviceID][i])
			return
		}
	}
	writeErr(w, http.StatusNotFound, "userDataNotFound", "no such variable")
}

// deleteUserData is DELETE /user-data/{id}: 200 with a process.
func (f *Fake) deleteUserData(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for serviceID, list := range f.userData {
		for i, e := range list {
			if e.ID != id {
				continue
			}
			if !f.userDataReachable(w, serviceID) {
				return
			}
			f.userData[serviceID] = append(list[:i:i], list[i+1:]...)
			f.sequence++
			proc := zerops.Process{ID: "proc-" + itoa(f.sequence), ServiceStackID: serviceID, Status: zerops.ProcessFinished, ActionName: "stack.userData.delete"}
			f.processes[proc.ID] = proc
			writeJSON(w, 200, proc)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "userDataNotFound", "no such variable")
}

// userDataReachable writes the refusal for a service the token does not reach
// — 403 on an ungranted project, 404 on no such service — and reports whether
// the caller may go on. Called with the lock held.
func (f *Fake) userDataReachable(w http.ResponseWriter, serviceID string) bool {
	projectID := f.projectOfService(serviceID)
	if projectID == "" {
		writeErr(w, http.StatusNotFound, "serviceStackNotFound", "no such service")
		return false
	}
	if f.Ungranted[projectID] {
		writeErr(w, http.StatusForbidden, "insufficientPermissions", "the token does not hold that project")
		return false
	}
	return true
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
	if f.FailImports {
		writeErr(w, 400, "invalidImportYaml", "the import could not be read: "+body.Yaml)
		return
	}
	if f.importBuild == "" {
		writeJSON(w, 200, map[string]any{"serviceStacks": []any{}})
		return
	}
	type process struct {
		ID string `json:"id"`
	}
	type stack struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Processes []process `json:"processes"`
	}
	var stacks []stack
	for _, line := range strings.Split(body.Yaml, "\n") {
		hostname, ok := strings.CutPrefix(strings.TrimSpace(line), "- hostname: ")
		if !ok {
			continue
		}
		f.sequence++
		service := zerops.Service{
			ID: "svc-imported-" + itoa(f.sequence), ProjectID: projectID, ClientID: f.ClientID,
			Name: hostname, Status: "READY_TO_DEPLOY", Created: f.Now,
		}
		if f.importBuild == zerops.ProcessFinished {
			service.Status = "ACTIVE"
		}
		f.services[projectID] = append(f.services[projectID], service)
		actsOn := []zerops.ProcessStack{{ID: service.ID, Name: hostname}}
		created := zerops.Process{ID: "proc-" + itoa(f.sequence) + "-create", ProjectID: projectID,
			Status: zerops.ProcessFinished, ActionName: "stack.create", Created: f.Now, ServiceStacks: actsOn}
		build := zerops.Process{ID: "proc-" + itoa(f.sequence) + "-build", ProjectID: projectID,
			Status: f.importBuild, ActionName: "stack.build", Created: f.Now, ServiceStacks: actsOn}
		f.processes[created.ID], f.processes[build.ID] = created, build
		stacks = append(stacks, stack{ID: service.ID, Name: hostname,
			Processes: []process{{ID: created.ID}, {ID: build.ID}}})
	}
	writeJSON(w, 200, map[string]any{"serviceStacks": stacks})
}

// projectProcesses is GET /project/{id}/process: every process the fake holds
// for the project, live and ended.
func (f *Fake) projectProcesses(w http.ResponseWriter, projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := []zerops.Process{}
	for _, process := range f.processes {
		if process.ProjectID == projectID {
			list = append(list, process)
		}
	}
	writeJSON(w, 200, map[string]any{"list": list})
}

// BuildImports makes every later import create its services and end their
// builds with status: FAILED is a build that could not fetch its inputs.
func (f *Fake) BuildImports(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importBuild = status
}

func (f *Fake) stopStart(w http.ResponseWriter, path string) {
	parts := strings.Split(strings.TrimPrefix(path, "/service-stack/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	stop := parts[1] == "stop"
	f.stopped[parts[0]] = stop
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

// IsStopped reports whether the service was last stopped.
func (f *Fake) IsStopped(serviceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped[serviceID]
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
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
