package gitea_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

func readFake(t *testing.T) (*giteatest.Fake, *gitea.Client) {
	t.Helper()
	f := giteatest.New(t)
	f.AddRepo("acme/group", "main")
	return f, f.Client()
}

func TestFileAtARef(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	f.AddFile("acme/group", "main", "environments.yaml", "version: 1\n")
	// The tier directories carry spaces and an em dash; every segment is
	// escaped on its own and the slashes survive.
	f.AddFile("acme/group", "main", "3 — Stage/import.yaml", "services: []\n")

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"a file at the root", "environments.yaml", "version: 1\n"},
		{"a file under a tier directory", "3 — Stage/import.yaml", "services: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.File(context.Background(), "acme", "group", tc.path, "main")
			if err != nil {
				t.Fatalf("File(%q): %v", tc.path, err)
			}
			if string(got) != tc.want {
				t.Fatalf("File(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	_, err := client.File(context.Background(), "acme", "group", "environments.yaml", "other")
	if !gitea.IsNotFound(err) {
		t.Fatalf("a file at a ref that has none = %v, want a 404", err)
	}
}

func TestDirListsTheTiers(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	f.AddFile("acme/group", "main", "environments.yaml", "version: 1\n")
	f.AddFile("acme/group", "main", "3 — Stage/import.yaml", "services: []\n")
	f.AddFile("acme/group", "main", "4 — Small Production/import.yaml", "services: []\n")

	entries, err := client.Dir(context.Background(), "acme", "group", "", "main")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	kinds := map[string]string{}
	for _, e := range entries {
		kinds[e.Name] = e.Type
	}
	want := map[string]string{
		"environments.yaml":    "file",
		"3 — Stage":            "dir",
		"4 — Small Production": "dir",
	}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Fatalf("%q came back as %q, want %q (all: %v)", name, kinds[name], kind, kinds)
		}
	}
}

func TestBranchHeadAndTags(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	f.SetBranch("acme/group", "main", "3f9c1b2e")
	f.AddTag("acme/group",
		gitea.Tag{Name: "v1.0.0", ID: "tagobj-1"},
		gitea.AnnotatedTag{Tag: "v1.0.0", SHA: "tagobj-1", Message: "api 3f9c1b2e\n"},
	)

	branch, err := client.Branch(context.Background(), "acme", "group", "main")
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if branch.Commit.ID != "3f9c1b2e" {
		t.Fatalf("main is at %q, want 3f9c1b2e", branch.Commit.ID)
	}
	if _, err := client.Branch(context.Background(), "acme", "group", "nope"); !gitea.IsNotFound(err) {
		t.Fatalf("a branch that does not exist = %v, want a 404", err)
	}

	tags, err := client.ListTags(context.Background(), "acme", "group")
	if err != nil || len(tags) != 1 || tags[0].Name != "v1.0.0" {
		t.Fatalf("ListTags = %v, %v", tags, err)
	}
	annotated, err := client.AnnotatedTag(context.Background(), "acme", "group", tags[0].ID)
	if err != nil {
		t.Fatalf("AnnotatedTag: %v", err)
	}
	if annotated.Message != "api 3f9c1b2e\n" {
		t.Fatalf("the tag's message is %q", annotated.Message)
	}
}

func TestArchiveIsBytesNotJSON(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	body := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00}
	f.SetArchive("acme/group", "3f9c1b2e", body)

	got, err := client.Archive(context.Background(), "acme", "group", "3f9c1b2e")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Archive = %v, want %v", got, body)
	}
	if _, err := client.Archive(context.Background(), "acme", "group", "deadbeef"); !gitea.IsNotFound(err) {
		t.Fatalf("an archive of a commit that does not exist = %v, want a 404", err)
	}
}

func TestAnnotatedTagCarriesItsTagger(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	when := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	f.AddTag("acme/group",
		gitea.Tag{Name: "v2.0.0", ID: "tagobj-2"},
		gitea.AnnotatedTag{
			Tag: "v2.0.0", SHA: "tagobj-2", Message: "api 77ab\nweb 88bc\n",
			Tagger: gitea.TagUser{Name: "u-abc", Email: "a@b.c", Date: when},
		},
	)
	got, err := client.AnnotatedTag(context.Background(), "acme", "group", "tagobj-2")
	if err != nil {
		t.Fatalf("AnnotatedTag: %v", err)
	}
	if !got.Tagger.Date.Equal(when) || got.Tagger.Name != "u-abc" {
		t.Fatalf("tagger = %+v", got.Tagger)
	}
}

// TestTagCommitPeelsBothKindsOfTag is the shape the release verdict depends on.
// Gitea's `create` webhook carries the peeled commit for a tag made through the
// API and the tag object's sha for one pushed by git, so the broker resolves the
// ref and peels it instead of trusting either.
func TestTagCommitPeelsBothKindsOfTag(t *testing.T) {
	t.Parallel()
	f, client := readFake(t)
	ctx := context.Background()

	annotated := gitea.Tag{Name: "v1.0.0", ID: "tagobject-1"}
	annotated.Commit.SHA = "commit-1"
	f.AddTag("acme/group", annotated, gitea.AnnotatedTag{
		Tag: "v1.0.0", SHA: "tagobject-1", Message: "api 3f9c\n",
	})
	f.AddLightweightTag("acme/group", "v2.0.0", "commit-2")

	for _, tc := range []struct {
		name string
		tag  string
		want string
	}{
		{"an annotated tag peels through its object", "v1.0.0", "commit-1"},
		{"a lightweight tag's ref is the commit", "v2.0.0", "commit-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.TagCommit(ctx, "acme", "group", tc.tag)
			if err != nil {
				t.Fatalf("TagCommit(%s): %v", tc.tag, err)
			}
			if got != tc.want {
				t.Fatalf("TagCommit(%s) = %q, want %q", tc.tag, got, tc.want)
			}
		})
	}

	if _, err := client.TagCommit(ctx, "acme", "group", "v9.9.9"); !gitea.IsNotFound(err) {
		t.Fatalf("a tag that does not exist = %v, want a 404", err)
	}
}

func TestPeelToCommitRefusesWhatItCannotPeel(t *testing.T) {
	t.Parallel()
	_, client := readFake(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		object gitea.GitObject
		want   string
	}{
		{"a commit is already peeled", gitea.GitObject{Type: "commit", SHA: "commit-1"}, ""},
		{"a tree is not a commit", gitea.GitObject{Type: "tree", SHA: "tree-1"}, "is not a commit or a tag"},
		{"a blob is not either", gitea.GitObject{Type: "blob", SHA: "blob-1"}, "is not a commit or a tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.PeelToCommit(ctx, "acme", "group", tc.object)
			if tc.want == "" {
				if err != nil || got != tc.object.SHA {
					t.Fatalf("PeelToCommit = %q, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PeelToCommit = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
