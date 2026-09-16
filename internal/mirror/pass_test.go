package mirror_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const (
	org        = "org-1"
	giteaPrjID = "p-gitea"
)

type rig struct {
	zerops *zeropstest.Fake
	gitea  *giteatest.Fake
	mirror *mirror.Mirror
}

// newRig is one org: a Gitea project carrying the registry, one Mate, one
// production project, an owner and a read-only member — and an empty Gitea.
func newRig(t *testing.T) *rig {
	t.Helper()
	z := zeropstest.New(t, org)
	z.Now = now
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	z.AddToken(zerops.Token{ID: "tok-broker", Name: "mate-broker", RoleCode: "READ_ONLY"})
	z.AddMember(zerops.Member{
		ID: "cu-owner", UserID: "u-owner", Status: "ACTIVE", RoleCode: "OWNER",
		User: zerops.UserLight{ID: "u-owner", Email: "owner@example", FullName: "Olga"},
	})
	z.AddMember(zerops.Member{
		ID: "cu-jan", UserID: "u-jan", Status: "ACTIVE", RoleCode: "READ_ONLY", CanCreateProjects: true,
		User: zerops.UserLight{ID: "u-jan", Email: "jan@example", FullName: "Jan"},
	})
	z.SetProjects(
		zerops.Project{ID: giteaPrjID, Name: "gitea", TagList: []string{
			"mate:tool:gitea",
			"mate:gn:g-acme:acme",
			"mate:gm:g-acme:p-fen:mate",
			"mate:gm:g-acme:p-prod:production",
		}},
		zerops.Project{ID: "p-fen", Name: "Fen", UserRoles: []zerops.UserRole{{ClientUserID: "cu-jan", RoleCode: "OWNER"}}},
		zerops.Project{ID: "p-prod", Name: "Acme production"},
	)

	g := giteatest.New(t)

	return &rig{
		zerops: z,
		gitea:  g,
		mirror: &mirror.Mirror{
			Zerops:         z.Client("broker"),
			Gitea:          g.Client(),
			Log:            slog.New(slog.DiscardHandler),
			ClientID:       org,
			GiteaProjectID: giteaPrjID,
			AdminLogin:     giteatest.AdminUser,
			HookURL:        hookURL,
			Cap:            10,
			Now:            func() time.Time { return now },
		},
	}
}

func TestPassBuildsAndThenChangesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	first, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Applied == 0 || first.Applied != first.Planned {
		t.Fatalf("first pass applied %d of %d; failures: %v", first.Applied, first.Planned, first.Failures)
	}

	// The org, its teams, its group repository and its hook exist.
	if got := r.gitea.Repos("acme"); len(got) != 1 || got[0] != "group" {
		t.Errorf("repos = %v", got)
	}
	if len(r.gitea.Hooks("acme")) != 1 {
		t.Errorf("hooks = %+v", r.gitea.Hooks("acme"))
	}
	if len(r.gitea.BranchRules("acme", "group")) != 2 || len(r.gitea.TagRules("acme", "group")) != 1 {
		t.Errorf("rules = %+v %+v", r.gitea.BranchRules("acme", "group"), r.gitea.TagRules("acme", "group"))
	}
	// The bot is a reader of its group.
	if got := r.gitea.TeamMembers("acme", "read"); !contains(got, "mate-p-fen") {
		t.Errorf("read team = %v", got)
	}
	if bot, ok := r.gitea.User("mate-p-fen"); !ok || !bot.Restricted {
		t.Errorf("bot = %+v, %v", bot, ok)
	}

	// A second pass on the state the first one produced plans nothing.
	second, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Planned != 0 {
		t.Errorf("the second pass plans %d actions:\n%s", second.Planned, mirror.Describe(second.Plan))
	}
}

// People arrive in Gitea through the OIDC source, so a pass before anyone has
// signed in has no account to put in a team — and says so rather than acting.
func TestPassPlacesPeopleOnceTheyHaveSignedIn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := r.gitea.TeamMembers("acme", "write"); contains(got, roles.Login("u-jan")) {
		t.Fatalf("jan is in the write team before signing in: %v", got)
	}

	// Their first sign-in makes the account; the next pass places them.
	r.gitea.AddUser(giteaUser(roles.Login("u-owner")))
	r.gitea.AddUser(giteaUser(roles.Login("u-jan")))

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	// Olga is an org OWNER: every team, and site admin.
	for _, team := range []string{"read", "write", "release"} {
		if got := r.gitea.TeamMembers("acme", team); !contains(got, roles.Login("u-owner")) {
			t.Errorf("the owner is not in %s: %v", team, got)
		}
	}
	if u, _ := r.gitea.User(roles.Login("u-owner")); !u.IsAdmin {
		t.Error("the org OWNER is not a site admin")
	}
	// Jan is READ_ONLY in the org and OWNER of the Mate: read and write, no release.
	if got := r.gitea.TeamMembers("acme", "write"); !contains(got, roles.Login("u-jan")) {
		t.Errorf("jan is not in the write team: %v", got)
	}
	if got := r.gitea.TeamMembers("acme", "release"); contains(got, roles.Login("u-jan")) {
		t.Errorf("jan releases: %v", got)
	}
	if u, _ := r.gitea.User(roles.Login("u-jan")); u.IsAdmin {
		t.Error("a READ_ONLY member became a site admin")
	}
}

func TestAReadThatFailsWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		bend func(r *rig)
	}{
		{
			name: "the member list is a 500",
			bend: func(r *rig) { r.zerops.Fail["GET /client/"+org+"/user/list"] = http.StatusInternalServerError },
		},
		{
			name: "the project search is a 401",
			bend: func(r *rig) { r.zerops.Fail["POST /project/search"] = http.StatusUnauthorized },
		},
		{
			name: "the project search comes back a page short of its declared total",
			bend: func(r *rig) { r.zerops.TruncateProjectSearch = true },
		},
		{
			name: "the member list is empty, which no live org is",
			bend: func(r *rig) { r.zerops.ClearMembers() },
		},
		{
			name: "the registry's own project is not in the project list",
			bend: func(r *rig) { r.mirror.GiteaProjectID = "p-missing" },
		},
		{
			name: "Gitea's user list is a 500",
			bend: func(r *rig) { r.gitea.Fail["GET /admin/users"] = http.StatusInternalServerError },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.bend(r)

			_, err := r.mirror.Pass(context.Background())
			if err == nil {
				t.Fatal("the pass succeeded")
			}
			if !errors.Is(err, mirror.ErrUnreadable) {
				t.Errorf("err = %v, want an unreadable-org error", err)
			}
			if r.gitea.Wrote() {
				t.Errorf("the pass wrote to Gitea: %v", r.gitea.Calls)
			}
			if r.zerops.Wrote() {
				t.Errorf("the pass wrote to Zerops: %v", r.zerops.Requests)
			}
		})
	}
}

func TestAPlanOverTheCapIsReportedNotApplied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Twelve people who once signed in and are no longer members: 24
	// destructive actions, against a cap of three.
	for i := range 12 {
		r.gitea.AddUser(giteaUser(roles.Login("u-gone" + string(rune('a'+i)))))
	}
	r.mirror.Cap = 3
	r.gitea.ResetCalls()

	result, err := r.mirror.Pass(ctx)
	if !errors.Is(err, mirror.ErrCapped) {
		t.Fatalf("err = %v, want a capped plan", err)
	}
	if result.Destructive <= 3 {
		t.Errorf("destructive = %d", result.Destructive)
	}
	if result.Applied != 0 {
		t.Errorf("a capped pass applied %d actions", result.Applied)
	}
	if r.gitea.Wrote() {
		t.Errorf("a capped pass wrote to Gitea: %v", r.gitea.Calls)
	}
	// The plan is in the result, so a person can read what it would have done.
	if mirror.Describe(result.Plan) == "" {
		t.Error("the capped result carries no plan to read")
	}
}

func TestAPlanInsideTheCapIsApplied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	r.gitea.AddUser(giteaUser(roles.Login("u-gone")))
	r.mirror.Cap = 10

	result, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if result.Destructive != 2 {
		t.Errorf("destructive = %d, want 2 (deactivate + delete tokens)", result.Destructive)
	}
	if u, _ := r.gitea.User(roles.Login("u-gone")); u.Active {
		t.Error("the departed person is still active")
	}
}

// Nothing a pass logs names a token. The result is counts.
func TestResultLogsCountsOnly(t *testing.T) {
	r := newRig(t)
	result, err := r.mirror.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	v := result.LogValue()
	if v.Kind().String() != "Group" {
		t.Fatalf("LogValue kind = %s", v.Kind())
	}
	for _, attr := range v.Group() {
		switch attr.Key {
		case "planned", "applied", "destructive", "problems", "failures", "awaiting_sign_in":
		default:
			t.Errorf("the log carries %q, which is not a count", attr.Key)
		}
	}
}

// A Gitea account is made at a person's first sign-in, so "has rights, has no
// account" is the ordinary state of everyone who has not signed in yet — the
// summary counts them. A person who has signed in and is missing from a team
// is a real diff, and the pass plans it.
func TestAPersonWithoutAnAccountIsCountedNotReported(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	first, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Problems) != 0 {
		t.Errorf("the pass reported %v", first.Problems)
	}
	// Olga and Jan both hold rights and neither has signed in.
	if first.AwaitingSignIn != 2 {
		t.Errorf("awaiting = %d, want the two people with rights and no account", first.AwaitingSignIn)
	}

	// They sign in; the next pass has accounts to put in teams.
	r.gitea.AddUser(giteaUser(roles.Login("u-owner")))
	r.gitea.AddUser(giteaUser(roles.Login("u-jan")))

	second, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.AwaitingSignIn != 0 {
		t.Errorf("awaiting = %d once everyone has an account", second.AwaitingSignIn)
	}
	if len(second.Problems) != 0 {
		t.Errorf("the pass reported %v", second.Problems)
	}
	// Missing from a team is a diff, not a note: the pass planned and applied
	// the memberships.
	if second.Planned == 0 || second.Applied != second.Planned {
		t.Fatalf("the pass planned %d and applied %d; failures: %v", second.Planned, second.Applied, second.Failures)
	}
	if got := r.gitea.TeamMembers("acme", "write"); !contains(got, roles.Login("u-jan")) {
		t.Errorf("write team = %v", got)
	}
}

func giteaUser(login string) gitea.User { return gitea.User{Login: login, Active: true} }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
