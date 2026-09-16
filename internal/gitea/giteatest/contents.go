package giteatest

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// The read side of a repository: files at a ref, branch heads, tags, annotated
// tag objects and commit archives. A test seeds a repository's tree and the
// broker reads it exactly as it reads a real Gitea.

// AddFile puts a file into a repository at a ref. The ref is a branch name or
// a commit sha; a read at either answers what was last put there.
func (f *Fake) AddFile(fullName, ref, filePath, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[fullName+"@"+ref+":"+strings.Trim(filePath, "/")] = body
}

// SetBranch points a branch at a commit.
func (f *Fake) SetBranch(fullName, branch, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[fullName+"@"+branch] = sha
}

// AddTag registers a tag and the annotated tag object behind it.
func (f *Fake) AddTag(fullName string, tag gitea.Tag, annotated gitea.AnnotatedTag) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[fullName] = append(f.tags[fullName], tag)
	f.annotated[fullName+"@"+tag.ID] = annotated
}

// SetArchive gives a commit its archive bytes.
func (f *Fake) SetArchive(fullName, sha string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archives[fullName+"@"+sha] = body
}

// Statuses reads back every status written on a commit.
func (f *Fake) Statuses(fullName, sha string) []gitea.CommitStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitea.CommitStatus(nil), f.statuses[fullName+"@"+sha]...)
}

// serveContents handles the read routes of one repository. It answers false
// when the path is none of them.
func (f *Fake) serveContents(w http.ResponseWriter, r *http.Request, full, rest string) bool {
	switch {
	case r.Method == http.MethodGet && (rest == "/contents" || strings.HasPrefix(rest, "/contents/")):
		f.contents(w, r, full, strings.TrimPrefix(strings.TrimPrefix(rest, "/contents"), "/"))
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/branches/"):
		f.branch(w, full, strings.TrimPrefix(rest, "/branches/"))
	case r.Method == http.MethodGet && rest == "/tags":
		f.mu.Lock()
		defer f.mu.Unlock()
		out := f.tags[full]
		if out == nil {
			out = []gitea.Tag{}
		}
		writeJSON(w, 200, out)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/git/tags/"):
		f.mu.Lock()
		defer f.mu.Unlock()
		tag, ok := f.annotated[full+"@"+strings.TrimPrefix(rest, "/git/tags/")]
		if !ok {
			fail(w, http.StatusNotFound, "no such tag object")
			return true
		}
		writeJSON(w, 200, tag)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/archive/"):
		f.archive(w, full, strings.TrimSuffix(strings.TrimPrefix(rest, "/archive/"), ".tar.gz"))
	default:
		return false
	}
	return true
}

func (f *Fake) contents(w http.ResponseWriter, r *http.Request, full, filePath string) {
	ref := r.URL.Query().Get("ref")
	filePath = strings.Trim(filePath, "/")

	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := full + "@" + ref + ":"

	if body, ok := f.files[prefix+filePath]; ok && filePath != "" {
		writeJSON(w, 200, gitea.Content{
			Name: path.Base(filePath), Path: filePath, Type: "file",
			SHA: blobSha(body), Size: int64(len(body)),
			Encoding: "base64", Content: base64.StdEncoding.EncodeToString([]byte(body)),
		})
		return
	}

	// A directory: every distinct first segment under filePath.
	seen := map[string]gitea.Content{}
	for key, body := range f.files {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rel := strings.TrimPrefix(key, prefix)
		if filePath != "" {
			if !strings.HasPrefix(rel, filePath+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, filePath+"/")
		}
		name, _, isDir := strings.Cut(rel, "/")
		entry := gitea.Content{Name: name, Path: strings.Trim(filePath+"/"+name, "/"), Type: "file", Size: int64(len(body))}
		if isDir {
			entry.Type = "dir"
			entry.Size = 0
		}
		seen[name] = entry
	}
	if len(seen) == 0 {
		fail(w, http.StatusNotFound, "no such file or directory")
		return
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]gitea.Content, 0, len(names))
	for _, name := range names {
		out = append(out, seen[name])
	}
	writeJSON(w, 200, out)
}

func (f *Fake) branch(w http.ResponseWriter, full, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, ok := f.branches[full+"@"+name]
	if !ok {
		fail(w, http.StatusNotFound, "no such branch")
		return
	}
	var out gitea.Branch
	out.Name = name
	out.Commit.ID = sha
	writeJSON(w, 200, out)
}

func (f *Fake) archive(w http.ResponseWriter, full, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.archives[full+"@"+sha]
	if !ok {
		fail(w, http.StatusNotFound, "no such commit")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	_, _ = w.Write(body)
}

// AddRepo registers a repository that already exists, with its default branch.
func (f *Fake) AddRepo(fullName, defaultBranch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owner, name, _ := strings.Cut(fullName, "/")
	if _, exists := f.orgs[owner]; !exists {
		f.orgs[owner] = &gitea.Org{ID: f.id(), Name: owner}
	}
	f.repos[fullName] = &gitea.Repo{
		ID: f.id(), Name: name, FullName: fullName, Private: true,
		DefaultBranch: defaultBranch,
		CloneURL:      f.srv.URL + "/" + fullName + ".git",
		HTMLURL:       f.srv.URL + "/" + fullName,
	}
}

// blobSha stands in for git's own: it changes when the body does, which is all
// a recipe-delta pass compares.
func blobSha(body string) string {
	sum := sha1.Sum([]byte(body))
	return hex.EncodeToString(sum[:])
}
