package deploy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

// Full shas, because a release line carries one: a short sha would deploy
// once and never compare equal to the app version's name again.
const (
	apiSha = "3f9c1b2e5d7a4c6f8e0b1d2a3c4f5e6d7a8b9c0d"
	webSha = "77ab0e1f2d3c4b5a69788796a5b4c3d2e1f0a9b8"
	oldSha = "1111111111111111111111111111111111111111"
)

func TestParseReleaseMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want map[string]string
		err  string
	}{
		{
			name: "the contract's example",
			body: "api " + apiSha + "\nweb " + webSha + "\n",
			want: map[string]string{"api": apiSha, "web": webSha},
		},
		{
			name: "blank lines are not lines",
			body: "\n\napi " + apiSha + "\n\n",
			want: map[string]string{"api": apiSha},
		},
		{
			name: "a short sha is refused",
			body: "api 3f9c1b2e\n",
			err:  "line 1",
		},
		{
			name: "a sentence someone wrote above the list is refused",
			body: "Release of the invoices work\napi " + apiSha + "\n",
			err:  "line 1",
		},
		{
			name: "a line with no sha is refused",
			body: "api\n",
			err:  "line 1",
		},
		{
			name: "a hostname that is not one is refused",
			body: "My API " + apiSha + "\n",
			err:  "line 1",
		},
		{
			name: "one service at two commits is refused",
			body: "api " + apiSha + "\napi " + webSha + "\n",
			err:  "a second time",
		},
		{
			name: "an empty message lists nothing",
			body: "\n",
			err:  "lists no services",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deploy.ParseReleaseMessage(tc.body)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ParseReleaseMessage = %v, %v, want an error mentioning %q", got, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReleaseMessage: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseReleaseMessage = %v, want %v", got, tc.want)
			}
			for service, sha := range tc.want {
				if got[service] != sha {
					t.Fatalf("%s -> %s, want %s", service, got[service], sha)
				}
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a    string
		b    string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.1.0", "v1.0.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", 1},
		{"v1.0.0", "release-1", 1},
		{"release-1", "release-2", 0},
		{"v0.1.0", "v1.0.0", -1},
		{"v10.0.0", "v9.0.0", 1},
	} {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := deploy.CompareSemver(tc.a, tc.b); got != tc.want {
				t.Fatalf("CompareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSortReleases(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   []deploy.Release
		want string
	}{
		{
			name: "the tagger's date decides",
			in: []deploy.Release{
				{Tag: "v2.0.0", When: base},
				{Tag: "v1.0.0", When: base.Add(time.Hour)},
			},
			want: "v1.0.0,v2.0.0",
		},
		{
			name: "semver breaks a tie in the date",
			in: []deploy.Release{
				{Tag: "v1.0.0", When: base},
				{Tag: "v1.2.0", When: base},
				{Tag: "v1.0.9", When: base},
			},
			want: "v1.2.0,v1.0.9,v1.0.0",
		},
		{
			name: "a rollback tag made later is the newest, whatever it is called",
			in: []deploy.Release{
				{Tag: "v3.0.0", When: base},
				{Tag: "v1.0.1", When: base.Add(time.Minute)},
			},
			want: "v1.0.1,v3.0.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deploy.SortReleases(tc.in)
			var order []string
			for _, r := range tc.in {
				order = append(order, r.Tag)
			}
			if strings.Join(order, ",") != tc.want {
				t.Fatalf("SortReleases = %v, want %s", order, tc.want)
			}
		})
	}
}

// releaseWorld is a group repo with three tags and their verdicts.
func releaseWorld(t *testing.T) (*giteatest.Fake, *gitea.Client) {
	t.Helper()
	g := giteatest.New(t)
	g.AddRepo("acme/group", "main")
	return g, g.Client()
}

func addTag(t *testing.T, g *giteatest.Fake, name, commit string, when time.Time, message string) {
	t.Helper()
	tag := gitea.Tag{Name: name, ID: "obj-" + name}
	tag.Commit.SHA = commit
	g.AddTag("acme/group", tag, gitea.AnnotatedTag{
		Tag: name, SHA: "obj-" + name, Message: message,
		Tagger: gitea.TagUser{Name: "u-abc", Date: when},
	})
}

func judge(t *testing.T, client *gitea.Client, commit, tag, state string) {
	t.Helper()
	if _, err := client.CreateStatus(context.Background(), "acme", "group", commit, gitea.NewStatus{
		Context: deploy.ReleaseContext(tag), State: state, Description: "by u-abc",
	}); err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
}

func TestNewestApprovedReadsTheStatusesNotTheTagList(t *testing.T) {
	t.Parallel()
	g, client := releaseWorld(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	addTag(t, g, "v1.0.0", "commit-1", base, "api "+oldSha+"\n")
	judge(t, client, "commit-1", "v1.0.0", deploy.ReleaseApproved)

	// The newest tag by date — and refused, so it is never "newest".
	addTag(t, g, "v2.0.0", "commit-2", base.Add(time.Hour), "api "+apiSha+"\n")
	judge(t, client, "commit-2", "v2.0.0", deploy.ReleaseRefused)

	// A tag nobody has judged yet is not approved either: silence is not
	// approval.
	addTag(t, g, "v3.0.0", "commit-3", base.Add(2*time.Hour), "api "+webSha+"\n")

	release, found, err := deploy.NewestApproved(ctx, client, "acme", "group")
	if err != nil || !found {
		t.Fatalf("NewestApproved = %v, %v, %v", release, found, err)
	}
	if release.Tag != "v1.0.0" || release.Services["api"] != oldSha {
		t.Fatalf("NewestApproved = %+v, want the only approved tag", release)
	}

	// A later approved tag wins.
	addTag(t, g, "v4.0.0", "commit-4", base.Add(3*time.Hour), "api "+apiSha+"\n")
	judge(t, client, "commit-4", "v4.0.0", deploy.ReleaseApproved)
	release, _, err = deploy.NewestApproved(ctx, client, "acme", "group")
	if err != nil || release.Tag != "v4.0.0" {
		t.Fatalf("NewestApproved = %+v, %v, want v4.0.0", release, err)
	}
}

func TestNewestApprovedSkipsAMessageItCannotRead(t *testing.T) {
	t.Parallel()
	g, client := releaseWorld(t)
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	addTag(t, g, "v1.0.0", "commit-1", base, "api "+apiSha+"\n")
	judge(t, client, "commit-1", "v1.0.0", deploy.ReleaseApproved)
	// Approved, newer, and its message is prose: it deploys nothing.
	addTag(t, g, "v2.0.0", "commit-2", base.Add(time.Hour), "the invoices release\n")
	judge(t, client, "commit-2", "v2.0.0", deploy.ReleaseApproved)

	release, found, err := deploy.NewestApproved(context.Background(), client, "acme", "group")
	if err != nil || !found || release.Tag != "v1.0.0" {
		t.Fatalf("NewestApproved = %+v, %v, %v", release, found, err)
	}
}

func TestNoApprovedTagIsNotAnError(t *testing.T) {
	t.Parallel()
	_, client := releaseWorld(t)
	release, found, err := deploy.NewestApproved(context.Background(), client, "acme", "group")
	if err != nil || found {
		t.Fatalf("NewestApproved = %+v, %v, %v", release, found, err)
	}
}

func TestJudgedReadsTheLastVerdict(t *testing.T) {
	t.Parallel()
	g, client := releaseWorld(t)
	ctx := context.Background()
	addTag(t, g, "v1.0.0", "commit-1", time.Now(), "api "+apiSha+"\n")

	if _, found, err := deploy.Judged(ctx, client, "acme", "group", "commit-1", "v1.0.0"); err != nil || found {
		t.Fatalf("an unjudged tag = %v, %v", found, err)
	}
	judge(t, client, "commit-1", "v1.0.0", deploy.ReleaseRefused)
	verdict, found, err := deploy.Judged(ctx, client, "acme", "group", "commit-1", "v1.0.0")
	if err != nil || !found || verdict != deploy.ReleaseRefused {
		t.Fatalf("Judged = %q, %v, %v", verdict, found, err)
	}
	// Another tag on the same commit is judged on its own.
	if _, found, _ := deploy.Judged(ctx, client, "acme", "group", "commit-1", "v1.0.1"); found {
		t.Fatal("a second tag on one commit inherited the first's verdict")
	}
}
