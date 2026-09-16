package zeropstest

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The deploy half of the fake: app versions, uploads, build-and-deploy,
// processes, app-code and enable-subdomain-access.
//
// It copies the two refusals the broker's code depends on:
//
//   - enable-subdomain-access is 400 before a service has ever deployed;
//   - a build the test marked as doomed ends its process FAILED and leaves the
//     app version BUILD_FAILED, which is what a broken repository looks like.

// AppVersionRecord is one app version the fake holds. Name is kept here and
// never served on an app-version route: the platform echoes it nowhere, and
// the fake would be lying if it did. It reaches a client exactly where the
// platform puts it — the service's own userData, while the version is ACTIVE.
type AppVersionRecord struct {
	zerops.AppVersion
	// Name is what the create was called with.
	Name string
	// Archive is what was uploaded (or promoted) into this version.
	Archive []byte
	// Yaml and Setup are what build-and-deploy was called with.
	Yaml  string
	Setup string
}

// AddAppVersion seeds an existing app version on a service — what a service
// that has already deployed looks like.
func (f *Fake) AddAppVersion(v zerops.AppVersion, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.ID == "" {
		v.ID = "ver-" + name
	}
	f.versions[v.ID] = &AppVersionRecord{AppVersion: v, Name: name, Archive: []byte("seeded " + name)}
	if v.Status == zerops.AppVersionActive {
		f.deployed[v.ServiceStackID] = true
	}
}

// AppVersions reads back one service's versions, newest first.
func (f *Fake) AppVersions(serviceID string) []AppVersionRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versionsOf(serviceID)
}

// versionsOf is called with the lock held.
func (f *Fake) versionsOf(serviceID string) []AppVersionRecord {
	var out []AppVersionRecord
	for _, v := range f.versions {
		if v.ServiceStackID == serviceID {
			out = append(out, *v)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Sequence > out[i].Sequence {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// FailBuild makes the build of the next app version named name end FAILED.
func (f *Fake) FailBuild(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doomed[name] = true
}

// SubdomainEnabled reports whether enable-subdomain-access landed on a service.
func (f *Fake) SubdomainEnabled(serviceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subdomains[serviceID]
}

func (f *Fake) serveDeploy(w http.ResponseWriter, r *http.Request, path, key string) bool {
	switch {
	case r.Method == "POST" && strings.HasPrefix(path, "/service-stack/") && strings.HasSuffix(path, "/app-version"):
		f.createAppVersion(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/service-stack/"), "/app-version"))
	case r.Method == "GET" && strings.HasPrefix(path, "/service-stack/") && !strings.Contains(strings.TrimPrefix(path, "/service-stack/"), "/"):
		f.service(w, strings.TrimPrefix(path, "/service-stack/"))
	case r.Method == "PUT" && strings.HasSuffix(path, "/enable-subdomain-access"):
		f.enableSubdomain(w, strings.TrimSuffix(strings.TrimPrefix(path, "/service-stack/"), "/enable-subdomain-access"))
	case r.Method == "PUT" && strings.HasPrefix(path, "/app-version/") && strings.HasSuffix(path, "/upload"):
		f.upload(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/app-version/"), "/upload"))
	case r.Method == "PUT" && strings.HasPrefix(path, "/app-version/") && strings.HasSuffix(path, "/build-and-deploy"):
		f.buildAndDeploy(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/app-version/"), "/build-and-deploy"))
	case r.Method == "GET" && strings.HasPrefix(path, "/app-version/") && strings.HasSuffix(path, "/app-code"):
		f.appCode(w, strings.TrimSuffix(strings.TrimPrefix(path, "/app-version/"), "/app-code"))
	case r.Method == "GET" && strings.HasPrefix(path, "/process/"):
		f.process(w, lastSegment(path))
	case r.Method == "DELETE" && strings.HasPrefix(path, "/service-stack/"):
		f.deleteService(w, lastSegment(path))
	default:
		_ = key
		return false
	}
	return true
}

func (f *Fake) createAppVersion(w http.ResponseWriter, r *http.Request, serviceID string) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence++
	v := &AppVersionRecord{
		AppVersion: zerops.AppVersion{
			ID:             "ver-" + itoa(f.sequence),
			ServiceStackID: serviceID,
			Status:         "UPLOADING",
			Sequence:       f.sequence,
		},
		Name: in.Name,
	}
	f.versions[v.ID] = v
	// The create does not echo the name.
	writeJSON(w, 200, v.AppVersion)
}

// service is GET /service-stack/{id}: the service, its own environment and the
// version it runs. appVersionName is served for the ACTIVE version alone,
// exactly as the platform does — an older version flips to BACKUP and its name
// is gone for good.
func (f *Fake) service(w http.ResponseWriter, serviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, list := range f.services {
		for _, s := range list {
			if s.ID != serviceID {
				continue
			}
			detail := zerops.ServiceDetail{Service: s}
			for _, record := range f.versionsOf(serviceID) {
				if record.Status != zerops.AppVersionActive {
					continue
				}
				active := record.AppVersion
				detail.ActiveAppVersion = &active
				detail.UserData = []zerops.ServiceUserData{
					{Key: "hostname", Content: s.Name},
					{Key: zerops.AppVersionNameKey, Content: record.Name},
				}
				break
			}
			writeJSON(w, 200, detail)
			return
		}
	}
	writeErr(w, 400, "serviceStackNotFound", "no such service")
}

func (f *Fake) upload(w http.ResponseWriter, r *http.Request, versionID string) {
	body := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	if !ok {
		writeErr(w, 404, "appVersionNotFound", "no such app version")
		return
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		writeErr(w, 400, "invalidUserInput", "the archive is application/octet-stream")
		return
	}
	v.Archive = body
	v.Status = "WAITING_TO_DEPLOY"
	w.WriteHeader(200)
}

func (f *Fake) buildAndDeploy(w http.ResponseWriter, r *http.Request, versionID string) {
	var in struct {
		Yaml  string `json:"zeropsYaml"`
		Setup string `json:"zeropsYamlSetup"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[versionID]
	if !ok {
		writeErr(w, 404, "appVersionNotFound", "no such app version")
		return
	}
	if in.Yaml == "" || in.Setup == "" {
		writeErr(w, 400, "zeropsYamlSetupNotFound", "both the yaml and the setup are required")
		return
	}
	v.Yaml, v.Setup = in.Yaml, in.Setup

	f.sequence++
	proc := zerops.Process{
		ID: "proc-" + itoa(f.sequence), ServiceStackID: v.ServiceStackID,
		Status: zerops.ProcessFinished, ActionName: "stack.build-and-deploy",
	}
	if f.doomed[v.Name] {
		proc.Status = zerops.ProcessFailed
		v.Status = zerops.AppVersionBuildFailed
	} else {
		v.Status = zerops.AppVersionActive
		for _, other := range f.versions {
			if other.ServiceStackID == v.ServiceStackID && other.ID != v.ID && other.Status == zerops.AppVersionActive {
				other.Status = zerops.AppVersionBackup
			}
		}
		f.deployed[v.ServiceStackID] = true
	}
	f.processes[proc.ID] = proc
	writeJSON(w, 200, proc)
}

func (f *Fake) appCode(w http.ResponseWriter, versionID string) {
	f.mu.Lock()
	_, ok := f.versions[versionID]
	f.mu.Unlock()
	if !ok {
		writeErr(w, 404, "appVersionNotFound", "no such app version")
		return
	}
	// A pre-signed URL: no Authorization header is sent to it, which is the
	// property the client's Download relies on.
	writeJSON(w, 200, map[string]string{"url": f.srv.URL + appCodePath + versionID})
}

// appCodePath is served outside the API prefix and without a token, as a
// pre-signed storage URL is.
const appCodePath = "/app-code-blob/"

func (f *Fake) appCodeBlob(w http.ResponseWriter, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[strings.TrimPrefix(path, appCodePath)]
	if !ok {
		http.Error(w, "no such blob", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(v.Archive)
}

func (f *Fake) process(w http.ResponseWriter, processID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	proc, ok := f.processes[processID]
	if !ok {
		writeErr(w, 404, "processNotFound", "no such process")
		return
	}
	writeJSON(w, 200, proc)
}

func (f *Fake) enableSubdomain(w http.ResponseWriter, serviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Measured three times: the call is 400 before the service has code.
	if !f.deployed[serviceID] {
		writeErr(w, 400, "serviceStackHasNoCode", "public access cannot be enabled before a deploy")
		return
	}
	f.subdomains[serviceID] = true
	for projectID, list := range f.services {
		for i, s := range list {
			if s.ID == serviceID {
				f.services[projectID][i].SubdomainAccess = true
			}
		}
	}
	writeJSON(w, 200, map[string]any{"id": serviceID})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// AddProcess seeds a process — what a deploy that is still moving looks like.
func (f *Fake) AddProcess(p zerops.Process) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processes[p.ID] = p
}

// deleteService is DELETE /service-stack/{id}.
func (f *Fake) deleteService(w http.ResponseWriter, serviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for projectID, list := range f.services {
		for i, s := range list {
			if s.ID != serviceID {
				continue
			}
			f.services[projectID] = append(append([]zerops.Service{}, list[:i]...), list[i+1:]...)
			f.DeletedServices = append(f.DeletedServices, serviceID)
			f.sequence++
			status := zerops.ProcessFinished
			if f.FailDeletes {
				status = zerops.ProcessFailed
			}
			proc := zerops.Process{ID: "proc-" + itoa(f.sequence), Status: status, ActionName: "stack.delete"}
			f.processes[proc.ID] = proc
			writeJSON(w, 200, proc)
			return
		}
	}
	writeErr(w, 400, "serviceStackNotFound", "no such service")
}

// Services reads back one project's services.
func (f *Fake) Services(projectID string) []zerops.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zerops.Service(nil), f.services[projectID]...)
}

// SetProjectTags replaces one project's tag list — what an owner writing the
// registry does.
func (f *Fake) SetProjectTags(projectID string, tags []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.projects {
		if f.projects[i].ID == projectID {
			f.projects[i].TagList = tags
		}
	}
}
