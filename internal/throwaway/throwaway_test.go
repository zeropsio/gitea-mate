package throwaway_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/throwaway"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const (
	org       = "org-1"
	giteaHost = "web-1234-3000.prg1.zerops.app"
)

// rig is a fake org with a broker token and one well-formed throwaway minted by
// an active member. Each case bends exactly one thing.
type rig struct {
	fake    *zeropstest.Fake
	checker *throwaway.Checker
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := zeropstest.New(t, org)

	f.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	f.AddToken(zerops.Token{ID: "tok-broker", Name: "mate-broker", RoleCode: "READ_ONLY", Created: f.Now})

	f.AddIdentity("throwaway", zeropstest.Identity{UserInfoID: "tok-throw", TokenID: "tok-throw", ClientID: org})
	f.AddToken(zerops.Token{
		ID:            "tok-throw",
		Name:          "gitea-signin:" + giteaHost + ":n0nce",
		RoleCode:      "NO_ACCESS",
		CreatedByUser: "u-jan",
		Created:       f.Now.Add(-30 * time.Second),
	})
	f.AddMember(zerops.Member{ID: "cu-jan", UserID: "u-jan", Status: "ACTIVE", RoleCode: "READ_ONLY", CanCreateProjects: true})

	return &rig{
		fake: f,
		checker: &throwaway.Checker{
			Broker:    f.Client("broker"),
			AsCaller:  f.Client,
			ClientID:  org,
			GiteaHost: giteaHost,
		},
	}
}

// retoken replaces the throwaway's metadata.
func (r *rig) retoken(mutate func(*zerops.Token)) {
	tok := zerops.Token{
		ID:            "tok-throw",
		Name:          "gitea-signin:" + giteaHost + ":n0nce",
		RoleCode:      "NO_ACCESS",
		CreatedByUser: "u-jan",
		Created:       r.fake.Now.Add(-30 * time.Second),
	}
	mutate(&tok)
	r.fake.AddToken(tok)
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name   string
		bend   func(r *rig)
		bearer string
		want   string // the reason, or "" for the happy path
	}{
		{
			name:   "a throwaway minted seconds ago by an active member proves them",
			bearer: "throwaway",
		},
		{
			name:   "a token the platform no longer knows",
			bearer: "deleted-seconds-ago",
			want:   throwaway.ReasonTokenDead,
		},
		{
			name:   "no bearer at all",
			bearer: "",
			want:   throwaway.ReasonTokenDead,
		},
		{
			name: "a token of another org cannot read this org's tokens",
			bend: func(r *rig) {
				r.fake.AddIdentity("stranger", zeropstest.Identity{UserInfoID: "tok-x", TokenID: "tok-x", ClientID: "org-2"})
			},
			bearer: "stranger",
			want:   throwaway.ReasonWrongOrg,
		},
		{
			name: "a token whose /user/info id is not a token id of this org",
			bend: func(r *rig) {
				r.fake.AddIdentity("personal", zeropstest.Identity{UserInfoID: "u-jan", TokenID: "u-jan", ClientID: org})
			},
			bearer: "personal",
			want:   throwaway.ReasonWrongOrg,
		},
		{
			name:   "a token carrying an org role",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.RoleCode = "READ_ONLY" }) },
			bearer: "throwaway",
			want:   throwaway.ReasonHasRights,
		},
		{
			name: "a token carrying a project grant",
			bend: func(r *rig) {
				r.retoken(func(tok *zerops.Token) {
					tok.Projects = []zerops.ProjectAccess{{ProjectID: "p-1", RoleCode: "BASIC_USER"}}
				})
			},
			bearer: "throwaway",
			want:   throwaway.ReasonHasRights,
		},
		{
			name:   "a token that may create projects",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.CanCreateProjects = true }) },
			bearer: "throwaway",
			want:   throwaway.ReasonHasRights,
		},
		{
			name:   "a token with a finance flag",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.CanViewFinances = true }) },
			bearer: "throwaway",
			want:   throwaway.ReasonHasRights,
		},
		{
			name:   "a door throwaway, named for a Mate and not for this Gitea",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.Name = "mate-door:p-fen:n0nce" }) },
			bearer: "throwaway",
			want:   throwaway.ReasonWrongName,
		},
		{
			name:   "a sign-in throwaway for another Gitea host",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.Name = "gitea-signin:other.example:n0nce" }) },
			bearer: "throwaway",
			want:   throwaway.ReasonWrongName,
		},
		{
			name:   "a throwaway minted an hour ago",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.Created = r.fake.Now.Add(-time.Hour) }) },
			bearer: "throwaway",
			want:   throwaway.ReasonStale,
		},
		{
			name:   "a throwaway dated in the future beyond the window",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.Created = r.fake.Now.Add(10 * time.Minute) }) },
			bearer: "throwaway",
			want:   throwaway.ReasonStale,
		},
		{
			name:   "a throwaway four minutes old is still fresh",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.Created = r.fake.Now.Add(-4 * time.Minute) }) },
			bearer: "throwaway",
		},
		{
			name:   "the creator has left the org",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.CreatedByUser = "u-gone" }) },
			bearer: "throwaway",
			want:   throwaway.ReasonNotMember,
		},
		{
			name: "the creator is in the list but not ACTIVE",
			bend: func(r *rig) {
				r.retoken(func(tok *zerops.Token) { tok.CreatedByUser = "u-invited" })
				r.fake.AddMember(zerops.Member{ID: "cu-inv", UserID: "u-invited", Status: "INVITED", RoleCode: "ADMIN"})
			},
			bearer: "throwaway",
			want:   throwaway.ReasonNotMember,
		},
		{
			name:   "a token naming no creator",
			bend:   func(r *rig) { r.retoken(func(tok *zerops.Token) { tok.CreatedByUser = "" }) },
			bearer: "throwaway",
			want:   throwaway.ReasonNotMember,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.bend != nil {
				tc.bend(r)
			}
			caller, err := r.checker.Check(context.Background(), tc.bearer)

			if tc.want == "" {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if caller.UserID != "u-jan" || caller.Member.RoleCode != "READ_ONLY" || caller.TokenID != "tok-throw" {
					t.Errorf("caller = %+v", caller)
				}
				return
			}
			var refusal *throwaway.Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Check = %v, want a refusal with reason %q", err, tc.want)
			}
			if refusal.Reason != tc.want {
				t.Errorf("reason = %q, want %q", refusal.Reason, tc.want)
			}
		})
	}
}

// The order matters: a token that fails several conditions is refused for the
// first one, so an attacker learns as little as possible and the log names the
// real problem.
func TestFirstFailingConditionWins(t *testing.T) {
	r := newRig(t)
	r.retoken(func(tok *zerops.Token) {
		tok.RoleCode = "ADMIN"            // step 3
		tok.Name = "something-else"       // step 4
		tok.Created = time.Time{}         // step 5
		tok.CreatedByUser = "u-not-there" // step 6
	})
	_, err := r.checker.Check(context.Background(), "throwaway")
	var refusal *throwaway.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != throwaway.ReasonHasRights {
		t.Errorf("reason = %v, want %s", err, throwaway.ReasonHasRights)
	}
}

// Trouble reaching Zerops is not a refusal: the caller may be perfectly
// legitimate and the answer must be the broker's fault, not theirs.
func TestBrokerTroubleIsNotARefusal(t *testing.T) {
	r := newRig(t)
	r.fake.Fail["GET /client/"+org+"/user/list"] = http.StatusInternalServerError

	_, err := r.checker.Check(context.Background(), "throwaway")
	if err == nil {
		t.Fatal("want an error")
	}
	var refusal *throwaway.Refusal
	if errors.As(err, &refusal) {
		t.Errorf("a 500 on the member list became a refusal: %v", err)
	}
}

func TestMissingAPIDateIsNotARefusal(t *testing.T) {
	// Without the API's clock the age cannot be judged, and guessing with the
	// container's would defeat the point of step 5.
	f := zeropstest.New(t, org)
	f.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	f.AddIdentity("throwaway", zeropstest.Identity{UserInfoID: "tok-throw", TokenID: "tok-throw", ClientID: org})
	f.AddToken(zerops.Token{
		ID: "tok-throw", Name: "gitea-signin:" + giteaHost + ":n", RoleCode: "NO_ACCESS",
		CreatedByUser: "u-jan", Created: f.Now,
	})
	f.Now = time.Time{} // an unparsable Date header

	c := &throwaway.Checker{Broker: f.Client("broker"), AsCaller: f.Client, ClientID: org, GiteaHost: giteaHost}
	_, err := c.Check(context.Background(), "throwaway")
	var refusal *throwaway.Refusal
	if err == nil || errors.As(err, &refusal) {
		t.Errorf("err = %v, want a plain error", err)
	}
}

func TestNamePrefix(t *testing.T) {
	c := &throwaway.Checker{GiteaHost: giteaHost}
	if got, want := c.NamePrefix(), "gitea-signin:"+giteaHost+":"; got != want {
		t.Errorf("NamePrefix() = %q, want %q", got, want)
	}
}
