package zerops_test

import (
	"testing"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

const (
	fullSha  = "7e2d4c1a9b3f5e6d8c0a1b2c3d4e5f6a7b8c9d0e"
	otherSha = "3f9c1b2e5cd0d39ae4e3c5b934829b22de8d0955"
)

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
		{"a prefix that is not hex never matches", "7e2d4c1-dirty", fullSha, false},
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
