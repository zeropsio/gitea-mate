package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/environments"
	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// The one place the broker runs `git`.
//
// An environment fed by several branches is realised as `env/{name}`, rebuilt
// by merging its sources in order whenever one moves. That is a merge of refs
// in a throwaway directory with hooks disabled — `-c core.hooksPath=/dev/null`
// — and nothing of the repository is executed: no checkout hook, no
// `.gitattributes` filter, no build. A conflict leaves the environment on its
// last good merge and is reported as a commit status on the head of the first
// source (docs/group-repo.md).

// ErrConflict is returned when the sources do not merge. The environment stays
// where it was.
var ErrConflict = errors.New("the sources do not merge")

// MergeContext is the commit status a failed merge is reported under.
func MergeContext(environment string) string { return "mate/merge/" + environment }

// Merger rebuilds an environment's branch. A single-source environment never
// reaches it: its head is a branch read, and no working copy is made at all.
type Merger struct {
	Gitea *gitea.Client
	Log   *slog.Logger
	// CloneURL builds the URL a working copy is cloned from, credentials
	// included. It is a function so the value never has to be stored, and so a
	// test can point it at a directory. It can fail: the credential in it is
	// the site admin's, which is resolved from the platform when it must be.
	CloneURL func(ctx context.Context, owner, repo string) (string, error)
	// Timeout bounds one merge.
	Timeout time.Duration
	// Dir is where throwaway working copies are made; empty means the
	// platform's temporary directory.
	Dir string
}

// DefaultMergeTimeout bounds one merge of one repository.
const DefaultMergeTimeout = 2 * time.Minute

func (m *Merger) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Head is an environment's desired commit in one service repository: the head
// of its single source, or the merge of several into `env/{name}`.
//
// A conflict answers the branch's last good merge — the environment stays
// where it was — together with [ErrConflict], so the caller reports it and
// deploys nothing new.
func (m *Merger) Head(ctx context.Context, owner, repo string, env environments.Environment) (string, error) {
	if len(env.Sources) == 0 {
		return "", fmt.Errorf("%s has no sources", env.Name)
	}
	if len(env.Sources) == 1 {
		branch, err := m.Gitea.Branch(ctx, owner, repo, env.Sources[0])
		if err != nil {
			return "", fmt.Errorf("%s/%s: %s: %w", owner, repo, env.Sources[0], err)
		}
		return branch.Commit.ID, nil
	}
	return m.merge(ctx, owner, repo, env)
}

// lastGood is the environment branch's current head, or empty when the branch
// does not exist yet.
func (m *Merger) lastGood(ctx context.Context, owner, repo, branch string) string {
	existing, err := m.Gitea.Branch(ctx, owner, repo, branch)
	if err != nil {
		return ""
	}
	return existing.Commit.ID
}

func (m *Merger) merge(ctx context.Context, owner, repo string, env environments.Environment) (string, error) {
	target := env.Branch()
	previous := m.lastGood(ctx, owner, repo, target)

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = DefaultMergeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp(m.Dir, "mate-merge-")
	if err != nil {
		return previous, fmt.Errorf("a working copy could not be made: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	run := func(args ...string) (string, error) { return git(ctx, dir, args...) }

	cloneURL, err := m.CloneURL(ctx, owner, repo)
	if err != nil {
		return previous, fmt.Errorf("%s/%s could not be cloned: %w", owner, repo, err)
	}
	if _, err := git(ctx, "", "clone", "--quiet", cloneURL, dir); err != nil {
		return previous, fmt.Errorf("%s/%s could not be cloned: %w", owner, repo, err)
	}
	// The broker is not a person and its merges are not authored by one.
	if _, err := run("config", "user.name", "mate-broker"); err != nil {
		return previous, err
	}
	if _, err := run("config", "user.email", "broker@bots.invalid"); err != nil {
		return previous, err
	}

	if _, err := run("checkout", "--quiet", "-B", target, "origin/"+env.Sources[0]); err != nil {
		return previous, fmt.Errorf("%s/%s: %s: %w", owner, repo, env.Sources[0], err)
	}
	for _, source := range env.Sources[1:] {
		if _, err := run("merge", "--no-edit", "--no-ff", "origin/"+source); err != nil {
			// Leave nothing half-merged behind; the branch keeps its last good
			// state either way, since nothing has been pushed.
			_, _ = run("merge", "--abort")
			m.log().Warn("an environment's sources do not merge",
				"environment", env.Name, "repo", owner+"/"+repo, "source", source)
			return previous, fmt.Errorf("%w: %s/%s: %s into %s", ErrConflict, owner, repo, source, env.Sources[0])
		}
	}

	head, err := run("rev-parse", "HEAD")
	if err != nil {
		return previous, err
	}
	head = strings.TrimSpace(head)
	if head == previous {
		return head, nil
	}
	if _, err := run("push", "--force-with-lease", "origin", target); err != nil {
		return previous, fmt.Errorf("%s/%s: %s could not be pushed: %w", owner, repo, target, err)
	}
	return head, nil
}

// git runs one command with every hook disabled and no interactive prompt. The
// merge is refs and trees; nothing of the repository's own is executed.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "advice.detachedHead=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+os.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The output can carry a clone URL, and a clone URL carries the admin
		// token, so only the command and the exit status are reported.
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}
