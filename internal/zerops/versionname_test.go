package zerops_test

import (
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// sha256 is a whole sha of a SHA-256 repository, which Gitea 1.27 can make.
var sha256 = strings.Repeat("7e2d4c1a", 8)

const (
	fullSha  = "7e2d4c1a9b3f5e6d8c0a1b2c3d4e5f6a7b8c9d0e"
	otherSha = "3f9c1b2e5cd0d39ae4e3c5b934829b22de8d0955"
)

func TestVersionName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		label string
		sha   string
		want  string
	}{
		{"a stage names its branch and the short sha", "main", fullSha, "main 7e2d4c1"},
		{"production names its tag and the short sha", "v0.1.0", fullSha, "v0.1.0 7e2d4c1"},
		{"a merged stage branch is a label like any other", "env/stage", fullSha, "env/stage 7e2d4c1"},
		{"a sha already short stays whole", "main", "3f9c", "main 3f9c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zerops.VersionName(tc.label, tc.sha); got != tc.want {
				t.Fatalf("VersionName(%q, %q) = %q, want %q", tc.label, tc.sha, got, tc.want)
			}
		})
	}
}

func TestVersionSha(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"an old stage version is the bare sha", fullSha, fullSha},
		{"an old production version puts the sha first", fullSha + " v1.2.0 u-abc", fullSha},
		{"an old production version whose tagger has spaces", fullSha + " v1.2.0 Gitea Admin", fullSha},
		{"a new stage version puts the short sha after the branch", "main 7e2d4c1", "7e2d4c1"},
		{"a new production version puts it after the tag", "v0.1.0 7e2d4c1", "7e2d4c1"},
		{"a branch that looks like hex is still the label", "deadbeefcafe 7e2d4c1", "7e2d4c1"},
		{"a two-word name a person typed is not ours", "hotfix friday", ""},
		{"a two-word name whose last word is too short to be a sha", "main 7e2d4c", ""},
		{"a version nobody named has no sha", "", ""},
		{"a name that starts with a space has none either", " v1", ""},
		{"a name with a doubled space is not ours", "main  7e2d4c1", ""},
		{"a name with a trailing space is not ours", "main 7e2d4c1 ", ""},
		{"zcp's push of a clean tree reads like a stage's", "main 7e2d4c1", "7e2d4c1"},
		{"zcp's push of a dirty tree is not a commit", "main 7e2d4c1-dirty", ""},
		{"a hand-made name ending in a date is not ours", "release 20260930", ""},
		{"a hand-made name ending in a timestamp is not ours", "deploy 1727712000", ""},
		{"a new name's sha is exactly seven hex", "main 7e2d4c1a", ""},
		{"a single word that is not a whole sha is not ours", "hotfix", ""},
		{"a single short sha is not ours", "7e2d4c1", ""},
		{"three words a person typed are not ours", "deploy 7e2d4c1 again", ""},
		{"an old production name whose tagger was empty", fullSha + " v1.2.0 ", fullSha},
		{"an old production name whose tagger has a doubled space", fullSha + " v1.2.0  Gitea Admin", fullSha},
		{"an old production name with no tagger at all", fullSha + " v1.2.0", fullSha},
		{"an old stage version of a SHA-256 repository", sha256, sha256},
		{"an old production version of a SHA-256 repository", sha256 + " v1.2.0 u-abc", sha256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zerops.VersionSha(tc.in); got != tc.want {
				t.Fatalf("VersionSha(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSameCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		token string
		sha   string
		want  bool
	}{
		{"the full sha is the commit", fullSha, fullSha, true},
		{"the seven-hex prefix is the commit", "7e2d4c1", fullSha, true},
		{"a longer prefix is the commit", "7e2d4c1a9b3f", fullSha, true},
		{"another commit's prefix is not", "3f9c1b2", fullSha, false},
		{"another full sha is not", otherSha, fullSha, false},
		{"a prefix shorter than seven never matches", "7e2d4c", fullSha, false},
		{"a dirty tree's short token is not the commit", "7e2d4c1-dirty", fullSha, false},
		{"a dirty tree's whole token is not the commit", fullSha + "-dirty", fullSha, false},
		{"a short commit is a spelling nothing can check", "7e2d4c1", "7e2d4c1a9", false},
		{"the seven-hex prefix of a SHA-256 commit", "7e2d4c1", sha256, true},
		{"a SHA-256 commit itself", sha256, sha256, true},
		{"a prefix of neither length is no commit", "7e2d4c1", sha256[:50], false},
		{"nothing deployed is no commit", "", fullSha, false},
		{"a short sha the fakes use still equals itself", "3f9c", "3f9c", true},
		{"a token longer than the sha is not its prefix", fullSha + "0", fullSha, false},
		{"upper-case hex is not how git writes a sha", "7E2D4C1", fullSha, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zerops.SameCommit(tc.token, tc.sha); got != tc.want {
				t.Fatalf("SameCommit(%q, %q) = %v, want %v", tc.token, tc.sha, got, tc.want)
			}
		})
	}
}
