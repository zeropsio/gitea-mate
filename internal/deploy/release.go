package deploy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// A release is a tag on the group repo that the broker approved when it
// arrived — never a tag it re-judges later. The approval is a commit status,
// so a restart, a catch-up pass and a `POST /deploy {production}` all read the
// same protected state, and a refused tag stays refused for ever
// (docs/group-repo.md, "Release tags").

// TagPrefix is what a release tag is named: `v{semver}`.
const TagPrefix = "v"

// The states a release status may carry.
const (
	ReleaseApproved = "success"
	ReleaseRefused  = "failure"
)

// ReleaseContext is the commit status a tag's verdict is recorded under. It
// names the tag, not just the commit: several tags may point at one commit,
// and each is judged on its own pusher.
func ReleaseContext(tag string) string { return "mate/release/" + tag }

// Release is an approved release tag and what it deploys.
type Release struct {
	// Tag is the tag's name, `v{semver}`.
	Tag string
	// Object is the annotated tag object's sha — what GET /git/tags/{sha}
	// takes; Commit is the commit it points at, where the verdict is written.
	Object string
	Commit string
	// Tagger is who made the tag, and When they made it. "Newest" among
	// approved tags is by this date, ties broken by semver.
	Tagger string
	When   time.Time
	// Services is the tag's message: a service hostname to the commit of its
	// repository that production should run.
	Services map[string]string
}

// fullSha is what a release line must carry. A short sha would deploy
// correctly once and then never compare equal to the app version's name, so
// the catch-up pass would redeploy it for ever.
var fullSha = regexp.MustCompile(`^[0-9a-f]{40}$`)

// serviceName is a Zerops hostname: lower-case letters and digits, starting
// with a letter (docs/vocabulary.md).
var serviceName = regexp.MustCompile(`^[a-z][a-z0-9]{0,39}$`)

// ParseReleaseMessage reads a tag's message: one `{service hostname} {full
// sha}` line per service, nothing else. A line it cannot read is refused
// rather than skipped — a release that half-parses would deploy half an app.
func ParseReleaseMessage(message string) (map[string]string, error) {
	out := map[string]string{}
	for n, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		service, sha, ok := strings.Cut(line, " ")
		sha = strings.TrimSpace(sha)
		if !ok || !serviceName.MatchString(service) || !fullSha.MatchString(sha) {
			return nil, fmt.Errorf("line %d is %q; a release lists `{service} {full sha}` and nothing else", n+1, line)
		}
		if other, dup := out[service]; dup && other != sha {
			return nil, fmt.Errorf("line %d names %s a second time, at another commit", n+1, service)
		}
		out[service] = sha
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the tag's message lists no services")
	}
	return out, nil
}

// Releases reads every `v*` tag of the group repo the broker approved, newest
// first. A tag with no verdict yet, one that was refused, and one whose
// message does not parse are all left out: production deploys what protected
// state says, and silence is not approval.
func Releases(ctx context.Context, g *gitea.Client, owner, repo string) ([]Release, error) {
	tags, err := g.ListTags(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: the tags: %w", owner, repo, err)
	}

	var out []Release
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Name, TagPrefix) {
			continue
		}
		commit := tag.Commit.SHA
		if commit == "" {
			continue
		}
		approved, err := Approved(ctx, g, owner, repo, commit, tag.Name)
		if err != nil {
			return nil, err
		}
		if !approved {
			continue
		}
		annotated, err := g.AnnotatedTag(ctx, owner, repo, tag.ID)
		if err != nil {
			if gitea.IsNotFound(err) {
				// A lightweight tag carries no message, so it lists nothing.
				continue
			}
			return nil, fmt.Errorf("%s/%s: tag %s: %w", owner, repo, tag.Name, err)
		}
		services, err := ParseReleaseMessage(annotated.Message)
		if err != nil {
			// A message that does not parse deploys nothing; the tag stays in
			// Gitea as a record.
			continue
		}
		out = append(out, Release{
			Tag: tag.Name, Object: tag.ID, Commit: commit,
			Tagger: annotated.Tagger.Name, When: annotated.Tagger.Date, Services: services,
		})
	}
	SortReleases(out)
	return out, nil
}

// NewestApproved is the release production runs: the first of [Releases].
func NewestApproved(ctx context.Context, g *gitea.Client, owner, repo string) (Release, bool, error) {
	all, err := Releases(ctx, g, owner, repo)
	if err != nil || len(all) == 0 {
		return Release{}, false, err
	}
	return all[0], true, nil
}

// Approved reads one tag's verdict off the tagged commit. The last status
// written under the tag's context wins, which is what a re-delivered webhook
// for an already-judged tag must not change.
func Approved(ctx context.Context, g *gitea.Client, owner, repo, commit, tag string) (bool, error) {
	statuses, err := g.ListStatuses(ctx, owner, repo, commit)
	if err != nil {
		return false, fmt.Errorf("%s/%s@%s: the statuses: %w", owner, repo, commit, err)
	}
	verdict := ""
	for _, s := range statuses {
		if s.Context == ReleaseContext(tag) {
			verdict = s.State
		}
	}
	return verdict == ReleaseApproved, nil
}

// Judged reports whether a tag already carries a verdict, and which. It is how
// a re-delivered webhook leaves a refusal alone.
func Judged(ctx context.Context, g *gitea.Client, owner, repo, commit, tag string) (string, bool, error) {
	statuses, err := g.ListStatuses(ctx, owner, repo, commit)
	if err != nil {
		return "", false, fmt.Errorf("%s/%s@%s: the statuses: %w", owner, repo, commit, err)
	}
	verdict, found := "", false
	for _, s := range statuses {
		if s.Context == ReleaseContext(tag) {
			verdict, found = s.State, true
		}
	}
	return verdict, found, nil
}

// SortReleases puts the newest first: by the tagger's date, ties broken by
// semver, and ties there by name — so the order never depends on what Gitea
// happened to list first.
func SortReleases(releases []Release) {
	sort.SliceStable(releases, func(i, j int) bool {
		a, b := releases[i], releases[j]
		if !a.When.Equal(b.When) {
			return a.When.After(b.When)
		}
		if c := CompareSemver(a.Tag, b.Tag); c != 0 {
			return c > 0
		}
		return a.Tag > b.Tag
	})
}

// semverPattern is `v{major}.{minor}.{patch}` with an optional pre-release.
var semverPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// CompareSemver orders two release tags: 1 when a is the later version, -1
// when b is, 0 when neither is. A tag that is not semver ranks below every one
// that is — the broker never invents an order for a name it does not
// understand.
func CompareSemver(a, b string) int {
	left, okLeft := parseSemver(a)
	right, okRight := parseSemver(b)
	switch {
	case !okLeft && !okRight:
		return 0
	case !okLeft:
		return -1
	case !okRight:
		return 1
	}
	for i := 0; i < 3; i++ {
		if left.number[i] != right.number[i] {
			if left.number[i] > right.number[i] {
				return 1
			}
			return -1
		}
	}
	// A release outranks its own pre-releases; two pre-releases compare as
	// strings, which is enough for the order of tags one account creates.
	switch {
	case left.pre == right.pre:
		return 0
	case left.pre == "":
		return 1
	case right.pre == "":
		return -1
	case left.pre > right.pre:
		return 1
	default:
		return -1
	}
}

type semver struct {
	number [3]int
	pre    string
}

func parseSemver(tag string) (semver, bool) {
	match := semverPattern.FindStringSubmatch(tag)
	if match == nil {
		return semver{}, false
	}
	var out semver
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return semver{}, false
		}
		out.number[i] = n
	}
	out.pre = match[4]
	return out, true
}
