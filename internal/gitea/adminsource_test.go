package gitea_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
)

// source is an AdminSource a test scripts: what it answers, what it answers
// after a refusal, and how often it was asked.
type source struct {
	mu      sync.Mutex
	creds   gitea.AdminCredentials
	after   *gitea.AdminCredentials // what a refusal turns into; nil keeps the pair
	err     error
	asked   int
	refused int
}

func (s *source) Admin(context.Context) (gitea.AdminCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	if s.err != nil {
		return gitea.AdminCredentials{}, s.err
	}
	return s.creds, nil
}

func (s *source) Refused(_ context.Context, used gitea.AdminCredentials) (gitea.AdminCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused++
	if s.after != nil && used == s.creds {
		s.creds = *s.after
	}
	return s.creds, nil
}

func (s *source) counts() (asked, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked, s.refused
}

func client(f *giteatest.Fake, src gitea.AdminSource) *gitea.Client {
	return gitea.New(gitea.Config{BaseURL: f.URL(), AdminUser: giteatest.AdminUser, Admin: src})
}

var (
	real  = gitea.AdminCredentials{Token: giteatest.AdminToken, Password: giteatest.AdminPassword}
	stale = gitea.AdminCredentials{Token: "stale-token", Password: "stale-password"}
)

// A 401 asks the source again and retries once with what it answers — the
// token route (basic auth) and every other route (the API token) alike.
func TestARefusalAsksTheSourceAgainAndRetriesOnce(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, c *gitea.Client) error
		path string
	}{
		{
			name: "an API-token call",
			call: func(ctx context.Context, c *gitea.Client) error { _, err := c.ListUsers(ctx); return err },
			path: "GET /admin/users",
		},
		{
			name: "a basic-auth call",
			call: func(ctx context.Context, c *gitea.Client) error {
				_, err := c.MintToken(ctx, giteatest.AdminUser, "t", []string{"all"})
				return err
			},
			path: "POST /users/admin/tokens",
		},
		{
			name: "an archive",
			call: func(ctx context.Context, c *gitea.Client) error {
				_, err := c.Archive(ctx, "acme", "group", "abc")
				return err
			},
			path: "GET /repos/acme/group/archive/abc.tar.gz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := giteatest.New(t)
			f.AddRepo("acme/group", "main")
			f.SetArchive("acme/group", "abc", []byte("tar"))
			src := &source{creds: stale, after: &real}
			if err := tc.call(context.Background(), client(f, src)); err != nil {
				t.Fatalf("the call failed after the source was refreshed: %v", err)
			}
			if asked, refused := src.counts(); asked != 1 || refused != 1 {
				t.Errorf("asked %d, refused %d; want 1 and 1", asked, refused)
			}
			var hits int
			for _, call := range f.Calls {
				if strings.HasPrefix(call, tc.path) {
					hits++
				}
			}
			if hits != 2 {
				t.Errorf("Gitea saw %s %d times, want 2 (the refusal and the retry): %v", tc.path, hits, f.Calls)
			}
		})
	}
}

// A source that hands back the pair Gitea just refused has nothing new to
// try: the 401 is the answer, and Gitea is not asked twice.
func TestTheSamePairIsNotRetried(t *testing.T) {
	f := giteatest.New(t)
	src := &source{creds: stale}
	_, err := client(f, src).ListUsers(context.Background())
	if gitea.Status(err) != 401 {
		t.Fatalf("err = %v, want the 401", err)
	}
	if _, refused := src.counts(); refused != 1 {
		t.Errorf("refused %d times, want 1", refused)
	}
	if len(f.Calls) != 1 {
		t.Errorf("Gitea saw %v, want one call", f.Calls)
	}
}

// A source that cannot answer — the platform could not be read — is an error
// the caller reports; Gitea is not called with nothing.
func TestASourceThatCannotAnswerIsTheCallsError(t *testing.T) {
	f := giteatest.New(t)
	boom := errors.New("the platform did not answer")
	src := &source{err: boom}
	_, err := client(f, src).ListUsers(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the source's", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("Gitea was called with no credentials: %v", f.Calls)
	}
}

// Credentials that arrive good are used as they are: one call, no refusal.
func TestGoodCredentialsCostNoExtraCall(t *testing.T) {
	f := giteatest.New(t)
	src := &source{creds: real}
	c := client(f, src)
	ctx := context.Background()
	if _, err := c.ListUsers(ctx); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if _, err := c.MintToken(ctx, giteatest.AdminUser, "t", []string{"all"}); err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if asked, refused := src.counts(); asked != 2 || refused != 0 {
		t.Errorf("asked %d, refused %d; want 2 and 0", asked, refused)
	}
}

// A client acting as an arbitrary token holds no source: a 401 there is that
// token's, and nothing is asked again.
func TestAsTokenHoldsNoSource(t *testing.T) {
	f := giteatest.New(t)
	src := &source{creds: real}
	_, err := client(f, src).AsToken("nobody").WhoAmI(context.Background())
	if gitea.Status(err) != 401 {
		t.Fatalf("err = %v, want the 401", err)
	}
	if asked, refused := src.counts(); asked != 0 || refused != 0 {
		t.Errorf("the source was asked %d and refused %d times; want never", asked, refused)
	}
}
