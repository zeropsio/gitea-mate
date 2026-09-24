package mirror_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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
	// zcpService is Fen's container: the service the pass writes the Mate's
	// Gitea access onto.
	zcpService = "s-fen-zcp"
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
	z.SetProjects(rigProjects(rigTags())...)
	// Fen's project: its zcp container, and the platform's own core stack,
	// which is never the one written to.
	z.SetServices("p-fen",
		zerops.Service{ID: "s-fen-core", ProjectID: "p-fen", Name: "core", Status: "ACTIVE", IsSystem: true,
			TypeInfo: zerops.ServiceTypeInfo{VersionName: "core@1"}},
		zerops.Service{ID: zcpService, ProjectID: "p-fen", Name: "zcp", Status: "ACTIVE",
			TypeInfo: zerops.ServiceTypeInfo{VersionName: "zcp@1"}},
	)

	g := giteatest.New(t)

	return &rig{
		zerops: z,
		gitea:  g,
		mirror: &mirror.Mirror{
			Zerops:          z.Client("broker"),
			Gitea:           g.Client(),
			Log:             slog.New(slog.DiscardHandler),
			ClientID:        org,
			GiteaProjectID:  giteaPrjID,
			AdminLogin:      giteatest.AdminUser,
			HookURL:         hookURL,
			GiteaPublicURL:  giteaPublicURL,
			BrokerPublicURL: brokerPublicURL,
			Cap:             10,
			Now:             func() time.Time { return now },
		},
	}
}

// rigTags is the registry the rig starts with: one group, acme, with Fen as
// its Mate and a production.
func rigTags() []string {
	return []string{
		"mate:tool:gitea",
		"mate:gn:g-acme:acme",
		"mate:gm:g-acme:p-fen:mate",
		"mate:gm:g-acme:p-prod:production",
	}
}

// rigProjects is the org's project list with the Gitea project carrying tags.
func rigProjects(tags []string) []zerops.Project {
	return []zerops.Project{
		{ID: giteaPrjID, Name: "gitea", TagList: tags},
		{ID: "p-fen", Name: "Fen", UserRoles: []zerops.UserRole{{ClientUserID: "cu-jan", RoleCode: "OWNER"}}},
		{ID: "p-prod", Name: "Acme production"},
	}
}

// registry replaces the registry the Gitea project carries.
func (r *rig) registry(tags ...string) { r.zerops.SetProjects(rigProjects(tags)...) }

// vars reads Fen's container back as key -> entry.
func (r *rig) vars() map[string]zerops.ServiceUserData {
	out := map[string]zerops.ServiceUserData{}
	for _, e := range r.zerops.UserData(zcpService) {
		out[e.Key] = e
	}
	return out
}

// touchedContainer reports which requests wrote a service variable or moved a
// container — the writes a delivery may and may not make.
func (r *rig) touchedContainer() (creates, updates, restarts int) {
	for _, req := range r.zerops.Requests {
		switch {
		case strings.HasPrefix(req, "POST /service-stack/") && strings.HasSuffix(req, "/user-data"):
			creates++
		case strings.HasPrefix(req, "PUT /user-data/"):
			updates++
		case strings.HasSuffix(req, "/restart"), strings.HasSuffix(req, "/stop"), strings.HasSuffix(req, "/start"),
			strings.HasSuffix(req, "/reload"):
			restarts++
		}
	}
	return creates, updates, restarts
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

// ---------------------------------------------------------------------------
// A Mate's Gitea access
// ---------------------------------------------------------------------------

// The first pass on a fresh Mate mints its bot's first generation and writes
// the three variables onto its container; the second pass touches nothing.
// The container is never restarted: zcp reads the live env store.
func TestPassDeliversAMatesAccessOnceAndNeverRestarts(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	first, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Failures) != 0 || len(first.Problems) != 0 {
		t.Fatalf("failures %v, problems %v", first.Failures, first.Problems)
	}

	names := r.gitea.Tokens("mate-p-fen")
	if len(names) != 1 || names[0] != mirror.TokenName("mate-p-fen", 1) {
		t.Fatalf("tokens = %v, want generation 1 alone", names)
	}
	vars := r.vars()
	if got := vars[mirror.VarGiteaURL]; got.Content != giteaPublicURL || got.Sensitive {
		t.Errorf("GITEA_URL = %+v", got)
	}
	if got := vars[mirror.VarBrokerURL]; got.Content != brokerPublicURL || got.Sensitive {
		t.Errorf("MATE_BROKER_URL = %+v", got)
	}
	token := vars[mirror.VarGiteaToken]
	if !token.Sensitive || token.Content == "" || !strings.HasSuffix(token.Content, "mate/mate-p-fen/1") {
		t.Errorf("GITEA_TOKEN = %+v, want the minted generation, sensitive", token)
	}
	if r.zerops.UserData("s-fen-core") != nil {
		t.Error("the platform's core stack was written to")
	}
	creates, updates, restarts := r.touchedContainer()
	if creates != 3 || updates != 0 || restarts != 0 {
		t.Errorf("creates %d updates %d restarts %d; want three creates and nothing else", creates, updates, restarts)
	}

	r.zerops.Requests = nil
	r.gitea.ResetCalls()
	second, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Planned != 0 {
		t.Errorf("the second pass plans:\n%s", mirror.Describe(second.Plan))
	}
	if r.zerops.Wrote() {
		t.Errorf("the second pass wrote to Zerops: %v", r.zerops.Requests)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Errorf("the second pass minted: %v", got)
	}
}

// A container holding another Gitea's access — the account was re-imported,
// or the Mate moved — gets a new generation and all three variables: the ones
// that exist are updated in place, the missing one created, and nothing is
// ever revoked on that pass.
func TestAnotherGiteasAccessIsReplacedInPlace(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", Active: true, Restricted: true})
	r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), "old-value-1", mirror.BotScopes...)
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{ID: "ud-url", Key: mirror.VarGiteaURL, Content: "https://elsewhere.example"},
		zerops.ServiceUserData{ID: "ud-tok", Key: mirror.VarGiteaToken, Content: "old-value-1", Sensitive: true},
	)

	result, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("failures %v", result.Failures)
	}

	names := r.gitea.Tokens("mate-p-fen")
	if len(names) != 2 || !contains(names, mirror.TokenName("mate-p-fen", 2)) || !contains(names, mirror.TokenName("mate-p-fen", 1)) {
		t.Fatalf("tokens = %v, want generations 1 and 2", names)
	}
	vars := r.vars()
	if got := vars[mirror.VarGiteaURL]; got.ID != "ud-url" || got.Content != giteaPublicURL {
		t.Errorf("GITEA_URL = %+v, want the same variable updated", got)
	}
	if got := vars[mirror.VarGiteaToken]; got.ID != "ud-tok" || !got.Sensitive || !strings.HasSuffix(got.Content, "mate/mate-p-fen/2") {
		t.Errorf("GITEA_TOKEN = %+v, want the same variable holding generation 2", got)
	}
	if got := vars[mirror.VarBrokerURL]; got.ID == "" || got.Content != brokerPublicURL {
		t.Errorf("MATE_BROKER_URL = %+v, want it created", got)
	}
	creates, updates, restarts := r.touchedContainer()
	if creates != 1 || updates != 2 || restarts != 0 {
		t.Errorf("creates %d updates %d restarts %d", creates, updates, restarts)
	}
}

// A container whose token is fine and whose broker URL is stale is written
// without a new generation.
func TestAStaleBrokerURLIsWrittenWithoutAMint(t *testing.T) {
	r := newRig(t)
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", Active: true, Restricted: true})
	r.gitea.AddToken("mate-p-fen", mirror.TokenName("mate-p-fen", 1), "value-1", mirror.BotScopes...)
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{Key: mirror.VarGiteaURL, Content: giteaPublicURL},
		zerops.ServiceUserData{Key: mirror.VarBrokerURL, Content: "https://old-broker.example"},
		zerops.ServiceUserData{Key: mirror.VarGiteaToken, Content: "value-1", Sensitive: true},
	)

	if _, err := r.mirror.Pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Errorf("a generation was minted for a URL fix: %v", got)
	}
	if got := r.vars()[mirror.VarBrokerURL].Content; got != brokerPublicURL {
		t.Errorf("MATE_BROKER_URL = %q", got)
	}
	if creates, updates, _ := r.touchedContainer(); creates != 0 || updates != 1 {
		t.Errorf("creates %d updates %d, want the one update", creates, updates)
	}
}

// A Mate the broker cannot serve yet is reported and skipped; the pass goes
// on for everything else and tries again next time.
func TestAnUnservableMateIsReportedNotFatal(t *testing.T) {
	cases := []struct {
		name string
		bend func(r *rig)
		want string
	}{
		{
			name: "the app has not granted the broker the project: 403",
			bend: func(r *rig) { r.zerops.Ungranted["p-fen"] = true },
			want: "granted",
		},
		{
			name: "the project has no zcp service yet",
			bend: func(r *rig) { r.zerops.SetServices("p-fen") },
			want: "zcp@",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.bend(r)

			result, err := r.mirror.Pass(context.Background())
			if err != nil {
				t.Fatalf("pass: %v", err)
			}
			// The rest of the pass happened: the org, its bot.
			if !r.gitea.Wrote() {
				t.Error("the pass wrote nothing to Gitea")
			}
			if _, ok := r.gitea.User("mate-p-fen"); !ok {
				t.Error("the bot was not made")
			}
			// Nothing was minted for a container that cannot be written, and
			// nothing was written to Zerops.
			if got := r.gitea.Tokens("mate-p-fen"); len(got) != 0 {
				t.Errorf("a generation was minted with nowhere to put it: %v", got)
			}
			if r.zerops.Wrote() {
				t.Errorf("the pass wrote to Zerops: %v", r.zerops.Requests)
			}
			var reported bool
			for _, p := range result.Problems {
				if strings.Contains(p, "p-fen") && strings.Contains(p, tc.want) {
					reported = true
				}
			}
			if !reported {
				t.Errorf("problems = %v, want one naming p-fen and %q", result.Problems, tc.want)
			}
			if len(result.Failures) != 0 {
				t.Errorf("failures = %v", result.Failures)
			}
		})
	}
}

// A delivery whose write fails is a failure of that action, not of the pass;
// the next pass mints again because the container does not hold the newest
// generation.
func TestAFailedWriteIsRetriedByMintingAgain(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.zerops.Fail["POST /service-stack/"+zcpService+"/user-data"] = http.StatusInternalServerError

	first, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Failures) != 1 || !strings.Contains(first.Failures[0], "deliver_mate_access") {
		t.Fatalf("failures = %v, want the delivery alone", first.Failures)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Fatalf("tokens = %v", got)
	}

	delete(r.zerops.Fail, "POST /service-stack/"+zcpService+"/user-data")
	second, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Failures) != 0 {
		t.Fatalf("failures = %v", second.Failures)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 2 {
		t.Errorf("tokens = %v, want generation 2 beside the orphaned 1", got)
	}
	if got := r.vars()[mirror.VarGiteaToken].Content; !strings.HasSuffix(got, "mate/mate-p-fen/2") {
		t.Errorf("GITEA_TOKEN holds %q, want generation 2", got)
	}
}

// D23 through the fake: a recipe pull request a Mate's bot opened on the group
// repo is merged by the next pass; the second pass finds nothing to do.
func TestAMatesRecipePullRequestIsMergedByThePass(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	r.gitea.AddPullRequest("acme/group", gitea.PullRequest{
		Number: 1, Title: "Mate: the group's import files",
		User: gitea.PullRequestUser{Login: "mate-p-fen"}, Base: gitea.PullRequestBranch{Ref: "main"},
	})
	r.gitea.AddPullRequest("acme/group", gitea.PullRequest{
		Number: 2, Title: "a person's change",
		User: gitea.PullRequestUser{Login: "u-jan"}, Base: gitea.PullRequestBranch{Ref: "main"},
	})

	second, err := r.mirror.Pass(ctx)
	if err != nil || len(second.Failures) != 0 {
		t.Fatalf("second pass: %v %v", err, second.Failures)
	}
	if pr, _ := r.gitea.PullRequest("acme/group", 1); !pr.Merged {
		t.Errorf("the Mate's recipe pull request was not merged: %+v", pr)
	}
	if pr, _ := r.gitea.PullRequest("acme/group", 2); pr.Merged {
		t.Errorf("a person's pull request was merged: %+v", pr)
	}
	third, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third.Planned != 0 {
		t.Errorf("the third pass plans:\n%s", mirror.Describe(third.Plan))
	}
}

// A recipe request main already carries is closed, not retried: Gitea calls
// it empty and answers every merge 405, and on the owner's org (2026-09-17)
// the loop failed the same merge every three minutes while the projects page
// offered the request for review.
func TestAnEmptyRecipePullRequestIsClosedNotRetried(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	r.gitea.AddPullRequest("acme/group", gitea.PullRequest{
		Number: 6, Title: "Mate: the group's import files",
		User: gitea.PullRequestUser{Login: "mate-p-fen"}, Base: gitea.PullRequestBranch{Ref: "main"},
	})
	r.gitea.SetPullRequestEmpty("acme/group", 6)

	second, err := r.mirror.Pass(ctx)
	if err != nil || len(second.Failures) != 0 {
		t.Fatalf("second pass: %v %v", err, second.Failures)
	}
	pr, _ := r.gitea.PullRequest("acme/group", 6)
	if pr.State != "closed" || pr.Merged {
		t.Fatalf("the empty request must be closed and not merged, got %+v", pr)
	}
	third, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third.Planned != 0 {
		t.Errorf("the third pass plans:\n%s", mirror.Describe(third.Plan))
	}
}

// A rule that exists is edited into shape, never created again: Gitea answers
// a duplicate with 403, and the owner's org kept main's release-only merge
// whitelist through every pass until this was measured (2026-09-17). Here a
// group repo carries the pre-D23 rule; one pass puts the current one in place
// and the next finds nothing to do.
func TestAnExistingBranchRuleIsEditedIntoShape(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	stale := gitea.BranchProtection{
		RuleName: "main", EnablePush: false, EnableMergeWhitelist: true,
		MergeWhitelistTeams: []string{"release"}, BlockAdminMergeOverride: true,
	}
	if _, err := r.gitea.Client().EditBranchProtection(ctx, "acme", "group", "main", stale); err != nil {
		t.Fatalf("staling the rule: %v", err)
	}

	second, err := r.mirror.Pass(ctx)
	if err != nil || len(second.Failures) != 0 {
		t.Fatalf("second pass: %v %v", err, second.Failures)
	}
	rules, err := r.gitea.Client().ListBranchProtections(ctx, "acme", "group")
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	var main *gitea.BranchProtection
	for i := range rules {
		if rules[i].RuleName == "main" {
			main = &rules[i]
		}
	}
	if main == nil || main.EnablePush || main.EnableMergeWhitelist || main.BlockAdminMergeOverride {
		t.Fatalf("main after the pass = %+v; want no direct push and no merge whitelist", main)
	}
	third, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third.Planned != 0 {
		t.Errorf("the third pass plans:\n%s", mirror.Describe(third.Plan))
	}
}

// ---------------------------------------------------------------------------
// The cap, per group
// ---------------------------------------------------------------------------

// overTheCapInAcme builds acme, puts four accounts nobody grants anything into
// its read team — four removals against a cap of three — and registers a
// second group, beta, which the next pass has to build.
func overTheCapInAcme(t *testing.T, r *rig) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	teams, err := r.gitea.Client().ListTeams(ctx, "acme")
	if err != nil {
		t.Fatalf("teams: %v", err)
	}
	var read int64
	for _, tm := range teams {
		if tm.Name == "read" {
			read = tm.ID
		}
	}
	for _, login := range []string{"stranger-a", "stranger-b", "stranger-c", "stranger-d"} {
		r.gitea.AddUser(gitea.User{Login: login, Active: true})
		if err := r.gitea.Client().AddTeamMember(ctx, read, login); err != nil {
			t.Fatalf("adding %s: %v", login, err)
		}
	}
	r.registry(append(rigTags(), "mate:gn:g-beta:beta")...)
	r.mirror.Cap = 3
}

// A group whose own plan takes away more than the cap is held; every other
// group of the org is served on the same pass (measured on the KRLS org
// 2026-09-23: one group's burst stopped rights for every group, pass after
// pass).
func TestAGroupOverTheCapDoesNotStopOtherGroupsActions(t *testing.T) {
	r := newRig(t)
	overTheCapInAcme(t, r)

	if _, err := r.mirror.Pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := r.gitea.Repos("beta"); !contains(got, "group") {
		t.Errorf("beta was not built while acme was over the cap: repos %v", got)
	}
}

// A bot's superseded generations — measured 2026-09-24: two Mates whose env
// write was refused for hours minted one generation every pass — are cleaned
// as one destructive unit of its group, whole, in one pass: never one unit per
// generation, which would hold the group once the pile outgrew the cap.
func TestABotWith40SupersededGenerationsIsCleanedInOnePassAndDoesNotHoldItsGroup(t *testing.T) {
	r := newRig(t)
	r.gitea.AddUser(giteaUser(roles.Login("u-jan")))
	r.gitea.AddUser(gitea.User{Login: "mate-p-fen", Active: true, Restricted: true})
	var newest string
	for gen := 1; gen <= 40; gen++ {
		name := mirror.TokenName("mate-p-fen", gen)
		newest = "value-" + name
		r.gitea.AddTokenCreated("mate-p-fen", name, newest, now.Add(-time.Duration(41-gen)*time.Hour), mirror.BotScopes...)
	}
	r.zerops.SetUserData(zcpService,
		zerops.ServiceUserData{Key: mirror.VarGiteaURL, Content: giteaPublicURL},
		zerops.ServiceUserData{Key: mirror.VarBrokerURL, Content: brokerPublicURL},
		zerops.ServiceUserData{Key: mirror.VarGiteaToken, Content: newest, Sensitive: true},
	)

	result, err := r.mirror.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := r.gitea.TeamMembers("acme", "write"); !contains(got, roles.Login("u-jan")) {
		t.Errorf("acme was held by its bot's cleanup: write team %v, problems %v", got, result.Problems)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 || got[0] != mirror.TokenName("mate-p-fen", 40) {
		t.Errorf("the bot keeps %v, want the newest alone", got)
	}
	if result.Destructive != 1 {
		t.Errorf("destructive = %d, want the cleanup counted as one unit", result.Destructive)
	}
}

// The held group gets none of its plan — not its removals, not its additions
// — and the pass names it as a problem, by group, never by token.
func TestAGroupOverTheCapAppliesNothingAndIsReported(t *testing.T) {
	r := newRig(t)
	overTheCapInAcme(t, r)
	// Jan signs in: acme's plan also puts him in its write team.
	r.gitea.AddUser(giteaUser(roles.Login("u-jan")))

	result, err := r.mirror.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := r.gitea.TeamMembers("acme", "read"); !contains(got, "stranger-a") {
		t.Errorf("a held group lost a member: read team %v", got)
	}
	if got := r.gitea.TeamMembers("acme", "write"); contains(got, roles.Login("u-jan")) {
		t.Errorf("a held group gained a member: write team %v", got)
	}
	var reported bool
	for _, p := range result.Problems {
		if strings.Contains(p, "acme") && strings.Contains(p, "cap") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("problems = %v, want one naming acme and the cap", result.Problems)
	}
}

// ---------------------------------------------------------------------------
// A project deleted in Zerops
// ---------------------------------------------------------------------------

// deleteFen deletes Fen's project in Zerops after a first pass served it: it
// leaves the project list, GET /project/p-fen answers 400 projectNotFound
// (measured), and its registry entry stays, because only the app writes tags.
func deleteFen(t *testing.T, r *rig) {
	t.Helper()
	if _, err := r.mirror.Pass(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	projects := rigProjects(rigTags())
	var kept []zerops.Project
	for _, p := range projects {
		if p.ID != "p-fen" {
			kept = append(kept, p)
		}
	}
	r.zerops.SetProjects(kept...)
	r.zerops.SetServices("p-fen")
}

// passAt runs a pass on the mirror's clock set to at.
func (r *rig) passAt(t *testing.T, at time.Time) mirror.Result {
	t.Helper()
	r.mirror.Now = func() time.Time { return at }
	result, err := r.mirror.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass at %s: %v", at, err)
	}
	return result
}

func TestAProjectMissingFromSearchAndNotFoundTwiceIsExcludedAndItsBotRetired(t *testing.T) {
	r := newRig(t)
	deleteFen(t, r)

	r.passAt(t, now)
	second := r.passAt(t, now.Add(mirror.DefaultInterval))

	for _, p := range second.Problems {
		if strings.Contains(p, "p-fen") {
			t.Errorf("a dead Mate is still planned for: %s", p)
		}
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 0 {
		t.Errorf("the dead Mate's bot keeps tokens %v", got)
	}
	if bot, _ := r.gitea.User("mate-p-fen"); !bot.ProhibitLogin {
		t.Errorf("the dead Mate's bot may still sign in: %+v", bot)
	}
	third := r.passAt(t, now.Add(2*mirror.DefaultInterval))
	if third.Planned != 0 {
		t.Errorf("a retired bot is planned again:\n%s", mirror.Describe(third.Plan))
	}
}

// Only projectNotFound counts. A refusal, an outage or a network error says
// nothing about whether the project exists, so the entry stays and nothing is
// retired, however long it lasts.
func TestA403OnTheProjectReadIsNeverTreatedAsDead(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := newRig(t)
			deleteFen(t, r)
			r.zerops.Fail["GET /project/p-fen"] = status

			for i := range 3 {
				r.passAt(t, now.Add(time.Duration(i)*mirror.DefaultInterval))
			}
			if got := r.gitea.Tokens("mate-p-fen"); len(got) == 0 {
				t.Error("the bot's tokens were deleted on a read that never said the project is gone")
			}
			if bot, _ := r.gitea.User("mate-p-fen"); bot.ProhibitLogin {
				t.Error("the bot was retired on a read that never said the project is gone")
			}
		})
	}
}

// One projectNotFound is not enough, and neither are two inside an interval:
// the search index and the write path disagree for a moment around a
// project's birth and death.
func TestASingleProjectNotFoundIsNotEnough(t *testing.T) {
	r := newRig(t)
	deleteFen(t, r)

	r.passAt(t, now)
	r.passAt(t, now.Add(mirror.DefaultInterval-time.Second))
	if got := r.gitea.Tokens("mate-p-fen"); len(got) == 0 {
		t.Error("the bot's tokens were deleted before a second sighting an interval later")
	}
	if bot, _ := r.gitea.User("mate-p-fen"); bot.ProhibitLogin {
		t.Error("the bot was retired before a second sighting an interval later")
	}
}

// Retirement is not one-way: a project listed again with its zcp container is
// alive, and its bot signs in again with the token the pass delivers.
func TestARetiredBotWhoseProjectReappearsCanSignInAgain(t *testing.T) {
	r := newRig(t)
	deleteFen(t, r)
	r.passAt(t, now)
	r.passAt(t, now.Add(mirror.DefaultInterval))
	if bot, _ := r.gitea.User("mate-p-fen"); !bot.ProhibitLogin {
		t.Fatalf("precondition: the bot was not retired: %+v", bot)
	}

	r.zerops.SetProjects(rigProjects(rigTags())...)
	r.zerops.SetServices("p-fen", zerops.Service{ID: zcpService, ProjectID: "p-fen", Name: "zcp", Status: "ACTIVE",
		TypeInfo: zerops.ServiceTypeInfo{VersionName: "zcp@1"}})
	r.passAt(t, now.Add(2*mirror.DefaultInterval))

	if bot, _ := r.gitea.User("mate-p-fen"); bot.ProhibitLogin {
		t.Errorf("the bot of a project that is back may not sign in: %+v", bot)
	}
	if got := r.gitea.Tokens("mate-p-fen"); len(got) != 1 {
		t.Errorf("tokens = %v, want the one generation delivered", got)
	}
}

// The 2026-09-23 incident on the KRLS org, replayed: seven Mates deleted in
// Zerops, their registry entries left behind in three groups, each bot
// holding a pile of token generations long past the grace — beside two live
// groups. The live groups are served on the first pass, the org is never
// capped as a whole, and by the second pass an interval later every dead bot
// is retired.
func TestTheDeletedMatesIncidentOf20260923(t *testing.T) {
	r := newRig(t)
	dead := map[string][]string{
		"old1": {"p-d1", "p-d2", "p-d3"},
		"old2": {"p-d4", "p-d5"},
		"old3": {"p-d6", "p-d7"},
	}
	tags := append(rigTags(), "mate:gn:g-beta:beta", "mate:gm:g-beta:p-bee:mate")
	for slug, ids := range dead {
		tags = append(tags, "mate:gn:g-"+slug+":"+slug)
		for _, id := range ids {
			tags = append(tags, "mate:gm:g-"+slug+":"+id+":mate")
			bot := mirror.BotLogin(id)
			r.gitea.AddUser(gitea.User{Login: bot, Active: true, Restricted: true})
			for gen := 1; gen <= 7; gen++ {
				name := mirror.TokenName(bot, gen)
				r.gitea.AddTokenCreated(bot, name, "value-"+name, now.Add(-time.Duration(8-gen)*time.Hour), mirror.BotScopes...)
			}
		}
	}
	r.zerops.SetProjects(append(rigProjects(tags), zerops.Project{ID: "p-bee", Name: "Bee"})...)
	r.zerops.SetServices("p-bee", zerops.Service{ID: "s-bee-zcp", ProjectID: "p-bee", Name: "zcp", Status: "ACTIVE",
		TypeInfo: zerops.ServiceTypeInfo{VersionName: "zcp@1"}})

	first := r.passAt(t, now)
	for _, slug := range []string{"acme", "beta"} {
		if got := r.gitea.Repos(slug); !contains(got, "group") {
			t.Errorf("live group %s was not built on the first pass: repos %v", slug, got)
		}
	}
	for _, svc := range []string{zcpService, "s-bee-zcp"} {
		var held bool
		for _, v := range r.zerops.UserData(svc) {
			held = held || v.Key == mirror.VarGiteaToken
		}
		if !held {
			t.Errorf("live Mate container %s holds no GITEA_TOKEN after the first pass; failures %v", svc, first.Failures)
		}
	}

	second := r.passAt(t, now.Add(mirror.DefaultInterval))
	for _, ids := range dead {
		for _, id := range ids {
			bot := mirror.BotLogin(id)
			if got := r.gitea.Tokens(bot); len(got) != 0 {
				t.Errorf("%s keeps %d tokens after the second pass", bot, len(got))
			}
			if u, _ := r.gitea.User(bot); !u.ProhibitLogin {
				t.Errorf("%s may still sign in after the second pass", bot)
			}
		}
	}
	for _, p := range second.Problems {
		if strings.Contains(p, "p-d") || strings.Contains(p, "old") {
			t.Errorf("a deleted Mate is still reported: %s", p)
		}
	}
	if len(second.Failures) != 0 {
		t.Errorf("failures on the second pass: %v", second.Failures)
	}
}

// The incident's other shape: a deleted Mate registered in a live group. Its
// pile of old generations falls due on the first pass, while it still reads
// as alive, and still never holds the group; by the second pass it is retired.
func TestADeletedMateInsideALiveGroupNeverHoldsIt(t *testing.T) {
	r := newRig(t)
	r.gitea.AddUser(giteaUser(roles.Login("u-jan")))
	r.registry(append(rigTags(), "mate:gm:g-acme:p-d8:mate")...)
	dead := mirror.BotLogin("p-d8")
	r.gitea.AddUser(gitea.User{Login: dead, Active: true, Restricted: true})
	for gen := 1; gen <= 12; gen++ {
		name := mirror.TokenName(dead, gen)
		r.gitea.AddTokenCreated(dead, name, "value-"+name, now.Add(-time.Duration(13-gen)*time.Hour), mirror.BotScopes...)
	}

	first := r.passAt(t, now)
	if got := r.gitea.TeamMembers("acme", "write"); !contains(got, roles.Login("u-jan")) {
		t.Errorf("acme was not served on the first pass: write team %v, problems %v", got, first.Problems)
	}

	r.passAt(t, now.Add(mirror.DefaultInterval))
	if got := r.gitea.Tokens(dead); len(got) != 0 {
		t.Errorf("%s keeps tokens %v after the second pass", dead, got)
	}
	if u, _ := r.gitea.User(dead); !u.ProhibitLogin {
		t.Errorf("%s may still sign in after the second pass", dead)
	}
}
