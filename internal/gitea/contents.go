package gitea

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The reads a deploy is decided from. All of them are reads: the broker never
// executes a repository's code, and it writes nothing here but a commit status.

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

// Content is one entry of GET /repos/{o}/{r}/contents/{path}. Type is "file",
// "dir", "symlink" or "submodule".
type Content struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	SHA         string `json:"sha"`
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	DownloadURL string `json:"download_url"`
}

// Decoded returns a file entry's bytes. Gitea answers base64 for a file it
// inlines and leaves content empty for one it does not.
func (c Content) Decoded() ([]byte, error) {
	if c.Encoding != "base64" {
		return nil, fmt.Errorf("gitea: %s came back %q-encoded, not base64", c.Path, c.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(c.Content)
	if err != nil {
		return nil, fmt.Errorf("gitea: %s: %w", c.Path, err)
	}
	return raw, nil
}

// contentsPath builds the route, with the ref as a query. A path segment may
// carry spaces and an em dash — the recipe's tier directories do — so every
// segment is escaped on its own and the slashes kept.
func contentsPath(owner, repo, path, ref string) string {
	route := "/repos/" + esc(owner) + "/" + esc(repo) + "/contents"
	if trimmed := strings.Trim(path, "/"); trimmed != "" {
		parts := strings.Split(trimmed, "/")
		for i, p := range parts {
			parts[i] = esc(p)
		}
		route += "/" + strings.Join(parts, "/")
	}
	if ref != "" {
		route += "?ref=" + esc(ref)
	}
	return route
}

// File reads one file of a repository at a ref. A missing file is a 404, which
// [IsNotFound] tells from a Gitea that could not be reached — the difference
// between "this group declares no environments" and "nothing is known".
func (c *Client) File(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	var entry Content
	if err := c.do(ctx, http.MethodGet, contentsPath(owner, repo, path, ref), nil, &entry, authToken); err != nil {
		return nil, err
	}
	if entry.Type != "file" {
		return nil, fmt.Errorf("gitea: %s/%s:%s is a %s, not a file", owner, repo, path, entry.Type)
	}
	return entry.Decoded()
}

// Dir lists one directory of a repository at a ref.
func (c *Client) Dir(ctx context.Context, owner, repo, path, ref string) ([]Content, error) {
	var entries []Content
	err := c.do(ctx, http.MethodGet, contentsPath(owner, repo, path, ref), nil, &entries, authToken)
	return entries, err
}

// ---------------------------------------------------------------------------
// Branches
// ---------------------------------------------------------------------------

// Branch is GET /repos/{o}/{r}/branches/{branch}. Its commit id is an
// environment's desired head when the environment follows one source.
type Branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Commit    struct {
		ID      string    `json:"id"`
		Message string    `json:"message"`
		Time    time.Time `json:"timestamp"`
	} `json:"commit"`
}

// Branch reads one branch. A branch that does not exist is a 404.
func (c *Client) Branch(ctx context.Context, owner, repo, branch string) (Branch, error) {
	var out Branch
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/branches/"+esc(branch), nil, &out, authToken)
	return out, err
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

// Tag is one row of GET /repos/{o}/{r}/tags. ID is the tag object's sha for an
// annotated tag — what GetAnnotatedTag is addressed by — and Commit.SHA the
// commit it points at.
type Tag struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Message string `json:"message"`
	Commit  struct {
		SHA string `json:"sha"`
		URL string `json:"url"`
	} `json:"commit"`
}

// ListTags reads every tag of a repository.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]Tag, error) {
	var all []Tag
	err := paged(func(page int) (int, error) {
		var out []Tag
		if err := c.do(ctx, http.MethodGet, withPage("/repos/"+esc(owner)+"/"+esc(repo)+"/tags", page), nil, &out, authToken); err != nil {
			return 0, err
		}
		all = append(all, out...)
		return len(out), nil
	})
	return all, err
}

// AnnotatedTag is GET /repos/{o}/{r}/git/tags/{sha}. The tagger's date is what
// orders approved releases (docs/group-repo.md, "Release tags"), and the
// message is the list of `{service} {sha}` lines.
type AnnotatedTag struct {
	Tag     string `json:"tag"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Object  struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
	Tagger TagUser `json:"tagger"`
}

// TagUser is who created an annotated tag, and when. The date is what orders
// approved releases; the name is a Gitea login only when the tag was made
// through the API as that person, which is how the Mate app makes one.
type TagUser struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

// AnnotatedTag reads one tag object by its sha — [Tag.ID], never the commit.
func (c *Client) AnnotatedTag(ctx context.Context, owner, repo, sha string) (AnnotatedTag, error) {
	var out AnnotatedTag
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/git/tags/"+esc(sha), nil, &out, authToken)
	return out, err
}

// ---------------------------------------------------------------------------
// Archives
// ---------------------------------------------------------------------------

// maxArchive bounds one repository archive. The broker moves it from Gitea to
// Zerops in memory; a repository larger than this is a problem to report.
const maxArchive = 512 << 20

// Archive is GET /repos/{o}/{r}/archive/{sha}.tar.gz — the bytes a deploy
// uploads. The archive deploys as-is because gitea/app.ini sets
// PREFIX_ARCHIVE_FILES=false: the platform keeps an archive's top-level
// folder, and a prefixed one fails its build (ledger 2026-09-15).
func (c *Client) Archive(ctx context.Context, owner, repo, sha string) ([]byte, error) {
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/archive/" + esc(sha) + ".tar.gz"
	_, raw, err := c.send(ctx, http.MethodGet, path, nil, authToken, maxArchive)
	return raw, err
}

// ---------------------------------------------------------------------------
// Peeling a tag to its commit
// ---------------------------------------------------------------------------

// GitObject is what a ref or a tag object points at.
type GitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
	URL  string `json:"url"`
}

// Reference is one row of GET /repos/{o}/{r}/git/refs/{ref}.
type Reference struct {
	Ref    string    `json:"ref"`
	Object GitObject `json:"object"`
}

// TagRef reads `refs/tags/{tag}`. Gitea answers a list for a prefix and a bare
// object for an exact ref depending on the version, so both are accepted.
func (c *Client) TagRef(ctx context.Context, owner, repo, tag string) (Reference, error) {
	path := "/repos/" + esc(owner) + "/" + esc(repo) + "/git/refs/tags/" + esc(tag)
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &raw, authToken); err != nil {
		return Reference{}, err
	}
	var list []Reference
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, ref := range list {
			if ref.Ref == "refs/tags/"+tag {
				return ref, nil
			}
		}
		if len(list) == 1 {
			return list[0], nil
		}
		return Reference{}, &APIError{Status: http.StatusNotFound, Message: "no ref refs/tags/" + tag, Path: "GET " + path}
	}
	var single Reference
	if err := json.Unmarshal(raw, &single); err != nil {
		return Reference{}, fmt.Errorf("gitea: GET %s: decode: %w", path, err)
	}
	return single, nil
}

// maxPeel bounds the walk from a ref to a commit. A tag of a tag of a tag is
// legal git and vanishingly rare; a cycle is not legal at all, and this is what
// stops one becoming a loop.
const maxPeel = 8

// PeelToCommit walks an object until it is a commit. An annotated tag's object
// is the commit; a tag of a tag takes one more hop.
func (c *Client) PeelToCommit(ctx context.Context, owner, repo string, object GitObject) (string, error) {
	for hop := 0; hop < maxPeel; hop++ {
		switch object.Type {
		case "commit":
			return object.SHA, nil
		case "tag":
			annotated, err := c.AnnotatedTag(ctx, owner, repo, object.SHA)
			if err != nil {
				return "", err
			}
			if annotated.Object.SHA == "" {
				return "", fmt.Errorf("gitea: the tag object %s points at nothing", object.SHA)
			}
			object = GitObject{Type: annotated.Object.Type, SHA: annotated.Object.SHA}
		default:
			return "", fmt.Errorf("gitea: %s/%s: a %s is not a commit or a tag", owner, repo, object.Type)
		}
	}
	return "", fmt.Errorf("gitea: %s/%s: a tag %d objects deep is not peeled", owner, repo, maxPeel)
}

// TagCommit is the commit a tag names, however the tag was made.
//
// Gitea 1.27.2's `create` webhook carries the peeled commit for a tag created
// through POST /repos/{o}/{r}/tags, and the tag object's sha for one pushed by
// git (measured 2026-09-16) — so the payload's sha is not a commit and cannot
// be treated as one. The ref is resolved and peeled instead, which answers the
// same commit either way.
func (c *Client) TagCommit(ctx context.Context, owner, repo, tag string) (string, error) {
	ref, err := c.TagRef(ctx, owner, repo, tag)
	if err != nil {
		return "", err
	}
	return c.PeelToCommit(ctx, owner, repo, ref.Object)
}
