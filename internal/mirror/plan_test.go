package mirror_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

const (
	adminLogin = "admin"
	hookURL    = "https://broker.example/hooks/gitea"
)

var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

const (
	giteaPublicURL  = "https://git.example"
	brokerPublicURL = "https://broker.example"
)

func opts() mirror.Options {
	return mirror.Options{
		AdminLogin: adminLogin, HookURL: hookURL, Now: now,
		GiteaPublicURL: giteaPublicURL, BrokerPublicURL: brokerPublicURL,
	}
}

// served is the Mate container of p-fen holding the three variables right for
// a token whose value is token.
func served(token string) map[string]mirror.MateService {
	return map[string]mirror.MateService{"p-fen": {
		ServiceID: "s-zcp",
		Vars: map[string]zerops.ServiceUserData{
			mirror.VarGiteaURL:   {ID: "ud-1", Key: mirror.VarGiteaURL, Content: giteaPublicURL},
			mirror.VarBrokerURL:  {ID: "ud-2", Key: mirror.VarBrokerURL, Content: brokerPublicURL},
			mirror.VarGiteaToken: {ID: "ud-3", Key: mirror.VarGiteaToken, Content: token, Sensitive: true},
		},
	}}
}

// liveToken is one live generation of p-fen's bot whose value ends in last8.
func liveToken(generation int, last8 string, age time.Duration) gitea.AccessToken {
	return gitea.AccessToken{Name: mirror.TokenName("mate-p-fen", generation), TokenLastEight: last8, CreatedAt: now.Add(-age)}
}

// oneGroup is a registry with one group, one Mate and a production project.
func oneGroup(t *testing.T) (registry.Registry, []registry.Problem) {
	t.Helper()
	return registry.Parse([]string{
		"mate:gn:g-acme:acme",
		"mate:gm:g-acme:p-fen:mate",
		"mate:gm:g-acme:p-prod:production",
	})
}

func emptyGitea() mirror.GiteaState {
	return mirror.GiteaState{
		Users:     map[string]gitea.User{adminLogin: {Login: adminLogin, IsAdmin: true, Active: true}},
		Orgs:      map[string]bool{},
		Teams:     map[string]map[string]mirror.TeamState{},
		Repos:     map[string]map[string]mirror.RepoState{},
		Hooks:     map[string][]gitea.Hook{},
		BotTokens: map[string][]gitea.AccessToken{},
	}
}

func kinds(p mirror.Plan) []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, string(a.Kind))
	}
	return out
}

func has(p mirror.Plan, want mirror.Action) bool {
	for _, a := range p.Actions {
		if a.Kind == want.Kind && a.Org == want.Org && a.Team == want.Team &&
			a.Login == want.Login && a.Repo == want.Repo && a.TokenName == want.TokenName {
			return true
		}
	}
	return false
}

func TestPlanBuildsAGroupFromNothing(t *testing.T) {
	reg, problems := oneGroup(t)
	state := mirror.State{
		Registry: reg, Problems: problems, Gitea: emptyGitea(),
		Mates: map[string]string{"p-fen": "Fen"},
	}
	plan := mirror.Compute(state, opts())

	for _, want := range []mirror.Action{
		{Kind: mirror.CreateOrg, Org: "acme"},
		{Kind: mirror.CreateTeam, Org: "acme", Team: "read"},
		{Kind: mirror.CreateTeam, Org: "acme", Team: "write"},
		{Kind: mirror.CreateTeam, Org: "acme", Team: "release"},
		{Kind: mirror.CreateRepo, Org: "acme", Repo: "group"},
		{Kind: mirror.CreateHook, Org: "acme"},
		{Kind: mirror.CreateBot, Org: "acme", Login: "mate-p-fen"},
		{Kind: mirror.ShapeBot, Org: "acme", Login: "mate-p-fen"},
		{Kind: mirror.AddTeamMember, Org: "acme", Team: "read", Login: "mate-p-fen"},
	} {
		if !has(plan, want) {
			t.Errorf("the plan lacks %s", want)
		}
	}

	// The order Gitea insists on: the org, then its teams, then the group
	// repository's rules — a protection naming a team that does not exist is
	// 422 (measured on 1.27.2).
	order := kinds(plan)
	if at(order, "create_org") > at(order, "create_team") {
		t.Error("teams are planned before their org")
	}
	if at(order, "create_team") > at(order, "set_branch_rule") {
		t.Error("the branch rules are planned before the teams they name")
	}
	if at(order, "create_repo") > at(order, "set_branch_rule") {
		t.Error("the branch rules are planned before the repository")
	}

	if plan.Destructive() != 0 {
		t.Errorf("building a group took %d things away", plan.Destructive())
	}
}

func at(kinds []string, kind string) int {
	for i, k := range kinds {
		if k == kind {
			return i
		}
	}
	return len(kinds) + 1
}

func TestGroupRepoProtections(t *testing.T) {
	reg, problems := oneGroup(t)
	plan := mirror.Compute(mirror.State{Registry: reg, Problems: problems, Gitea: emptyGitea()}, opts())

	var main, env *gitea.BranchProtection
	var tags *gitea.TagProtection
	for _, a := range plan.Actions {
		if a.BranchRule != nil {
			switch a.BranchRule.RuleName {
			case "main":
				main = a.BranchRule
			case "env/*":
				env = a.BranchRule
			}
		}
		if a.TagRule != nil {
			tags = a.TagRule
		}
	}
	if main == nil || env == nil || tags == nil {
		t.Fatalf("rules = %+v %+v %+v", main, env, tags)
	}
	if main.EnablePush {
		t.Error("main allows a direct push")
	}
	if main.EnableMergeWhitelist || len(main.MergeWhitelistTeams) != 0 || main.BlockAdminMergeOverride {
		// D23: anyone with write merges — the write and release teams, and the
		// broker landing a Mate's proposal. Until 2026-09-17 the release team
		// alone could, and every Mate's recipe waited for a releaser.
		t.Errorf("main merges = %+v; the group repo takes merges from anyone with write", main)
	}
	if !env.EnablePushWhitelist || len(env.PushWhitelistUsers) != 1 || env.PushWhitelistUsers[0] != adminLogin {
		t.Errorf("env/* pushers = %+v; the broker alone writes env/*", env.PushWhitelistUsers)
	}
	if tags.NamePattern != "v*" || len(tags.WhitelistTeams) != 1 || tags.WhitelistTeams[0] != "release" {
		t.Errorf("tag rule = %+v", tags)
	}
}

// A second pass on the state the first one produced plans nothing. This is the
// property that makes a reconcile safe to run every three minutes.
func TestPlanIsIdempotent(t *testing.T) {
	reg, problems := oneGroup(t)
	state := mirror.State{
		Registry: reg, Problems: problems,
		Mates: map[string]string{"p-fen": "Fen"},
		Members: []mirror.Member{
			{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
			{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
		},
		Gitea:        applied(t),
		MateServices: served("tok-11111111"),
	}
	plan := mirror.Compute(state, opts())
	if len(plan.Actions) != 0 {
		t.Errorf("a second pass plans %d actions:\n%s", len(plan.Actions), mirror.Describe(plan))
	}
	if len(plan.Problems) != 0 {
		t.Errorf("a second pass reports %v", plan.Problems)
	}
}

// applied is the Gitea state a first pass would leave behind for oneGroup plus
// an owner and a read-only member.
func applied(t *testing.T) mirror.GiteaState {
	t.Helper()
	g := emptyGitea()
	g.Users[roles.Login("u-owner")] = gitea.User{Login: roles.Login("u-owner"), Active: true, IsAdmin: true}
	g.Users[roles.Login("u-jan")] = gitea.User{Login: roles.Login("u-jan"), Active: true}
	g.Users["mate-p-fen"] = gitea.User{Login: "mate-p-fen", Active: true, Restricted: true}
	g.Orgs["acme"] = true
	g.Teams["acme"] = map[string]mirror.TeamState{
		"read": {ID: 1, Members: map[string]bool{
			roles.Login("u-owner"): true, roles.Login("u-jan"): true, "mate-p-fen": true,
		}},
		"write":   {ID: 2, Members: map[string]bool{roles.Login("u-owner"): true}},
		"release": {ID: 3, Members: map[string]bool{roles.Login("u-owner"): true}},
	}
	g.Repos["acme"] = map[string]mirror.RepoState{"group": {
		BranchRules: map[string]gitea.BranchProtection{
			"main": {RuleName: "main", EnablePush: false},
			"env/*": {
				RuleName: "env/*", EnablePush: true, EnablePushWhitelist: true,
				PushWhitelistUsers: []string{adminLogin},
			},
		},
		TagRules: map[string]gitea.TagProtection{
			"v*": {NamePattern: "v*", WhitelistTeams: []string{"release"}},
		},
	}}
	g.Hooks["acme"] = []gitea.Hook{{ID: 9, Config: map[string]string{"url": hookURL}}}
	g.BotTokens["mate-p-fen"] = []gitea.AccessToken{liveToken(1, "11111111", time.Hour)}
	return g
}

func TestTeamMembershipFollowsTheRoleFunction(t *testing.T) {
	reg, problems := oneGroup(t)
	base := applied(t)

	cases := []struct {
		name     string
		member   mirror.Member
		override map[string]roles.Role
		want     map[string]bool // team -> should be in it
	}{
		{
			name:   "an org owner is in all three",
			member: mirror.Member{UserID: "u-new", Email: "n@x", Status: "ACTIVE", RoleCode: roles.Owner},
			want:   map[string]bool{"read": true, "write": true, "release": true},
		},
		{
			name:   "an org admin is in all three but is not site admin (D10)",
			member: mirror.Member{UserID: "u-new", Email: "n@x", Status: "ACTIVE", RoleCode: roles.Admin},
			want:   map[string]bool{"read": true, "write": true, "release": true},
		},
		{
			name:   "a plain read-only member reads only",
			member: mirror.Member{UserID: "u-new", Email: "n@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
			want:   map[string]bool{"read": true},
		},
		{
			name:     "a read-only member who owns a Mate writes the group, does not release (D11)",
			member:   mirror.Member{UserID: "u-new", Email: "n@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
			override: map[string]roles.Role{"p-fen": roles.Owner},
			want:     map[string]bool{"read": true, "write": true},
		},
		{
			name:     "a releaser holds BASIC_USER on production",
			member:   mirror.Member{UserID: "u-new", Email: "n@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
			override: map[string]roles.Role{"p-prod": roles.BasicUser},
			want:     map[string]bool{"read": true, "write": true, "release": true},
		},
		{
			name:   "an invited person is in nothing",
			member: mirror.Member{UserID: "u-new", Email: "n@x", Status: "INVITED", RoleCode: roles.Admin},
			want:   map[string]bool{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := base
			g.Users = cloneUsers(base.Users)
			g.Users[roles.Login("u-new")] = gitea.User{Login: roles.Login("u-new"), Active: true}

			state := mirror.State{
				Registry: reg, Problems: problems,
				Mates: map[string]string{"p-fen": "Fen"},
				Members: append([]mirror.Member{
					{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
					{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
				}, tc.member),
				Overrides: map[string]map[string]roles.Role{"u-new": tc.override},
				Gitea:     g,
			}
			plan := mirror.Compute(state, opts())

			for _, team := range []string{"read", "write", "release"} {
				want := tc.want[team]
				got := has(plan, mirror.Action{Kind: mirror.AddTeamMember, Org: "acme", Team: team, Login: roles.Login("u-new")})
				if got != want {
					t.Errorf("team %s: planned add = %v, want %v\n%s", team, got, want, mirror.Describe(plan))
				}
			}
		})
	}
}

// settled is the member list that matches applied(): the two people whose Gitea
// accounts it already carries.
func settled() []mirror.Member {
	return []mirror.Member{
		{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
		{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
	}
}

func cloneUsers(in map[string]gitea.User) map[string]gitea.User {
	out := make(map[string]gitea.User, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestSiteAdminIsOrgOwnersOnly(t *testing.T) {
	reg, problems := oneGroup(t)
	g := applied(t)
	g.Users = cloneUsers(g.Users)
	// An org admin who is site admin in Gitea must be demoted (D10); the
	// broker's own admin account is never touched.
	g.Users[roles.Login("u-admin")] = gitea.User{Login: roles.Login("u-admin"), Active: true, IsAdmin: true}
	g.Users[roles.Login("u-owner2")] = gitea.User{Login: roles.Login("u-owner2"), Active: true, IsAdmin: false}

	state := mirror.State{
		Registry: reg, Problems: problems,
		Mates: map[string]string{"p-fen": "Fen"},
		Members: []mirror.Member{
			{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
			{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
			{UserID: "u-admin", Email: "a@x", Status: "ACTIVE", RoleCode: roles.Admin},
			{UserID: "u-owner2", Email: "o2@x", Status: "ACTIVE", RoleCode: roles.Owner},
		},
		Gitea: g,
	}
	plan := mirror.Compute(state, opts())

	if !has(plan, mirror.Action{Kind: mirror.DemoteSiteAdmin, Login: roles.Login("u-admin")}) {
		t.Error("an org ADMIN kept site admin")
	}
	if !has(plan, mirror.Action{Kind: mirror.PromoteSiteAdmin, Login: roles.Login("u-owner2")}) {
		t.Error("an org OWNER was not made site admin")
	}
	for _, a := range plan.Actions {
		if a.Login == adminLogin {
			t.Errorf("the plan touches the broker's own admin account: %s", a)
		}
	}
}

func TestDeparturesAreDisabledNotDeleted(t *testing.T) {
	reg, problems := oneGroup(t)
	g := applied(t)
	g.Users = cloneUsers(g.Users)
	g.Users[roles.Login("u-gone")] = gitea.User{Login: roles.Login("u-gone"), Active: true}

	state := mirror.State{
		Registry: reg, Problems: problems,
		Mates: map[string]string{"p-fen": "Fen"},
		// u-gone is not in the member list at all.
		Members: []mirror.Member{
			{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
			{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
		},
		Gitea: g,
	}
	plan := mirror.Compute(state, opts())

	if !has(plan, mirror.Action{Kind: mirror.DeactivatePerson, Login: roles.Login("u-gone")}) {
		t.Error("a departed person stayed active")
	}
	if !has(plan, mirror.Action{Kind: mirror.DeletePersonTokens, Login: roles.Login("u-gone")}) {
		t.Error("a departed person kept their tokens")
	}
	for _, a := range plan.Actions {
		if strings.Contains(string(a.Kind), "delete_user") {
			t.Errorf("the plan deletes a person: %s", a)
		}
		if a.Login == "mate-p-fen" && a.Kind == mirror.DeactivatePerson {
			t.Error("a bot was mistaken for a departed person")
		}
	}
}

// An integration token is a member of the org with an identity of its own
// (ledger 2026-09-15). No Gitea account is ever made for one, and it is never
// mistaken for a person who left.
func TestIntegrationTokensAreNotPeople(t *testing.T) {
	reg, problems := oneGroup(t)
	state := mirror.State{
		Registry: reg, Problems: problems,
		Mates: map[string]string{"p-fen": "Fen"},
		Members: []mirror.Member{
			{UserID: "u-owner", Email: "o@x", Status: "ACTIVE", RoleCode: roles.Owner},
			{UserID: "u-jan", Email: "j@x", Status: "ACTIVE", RoleCode: roles.ReadOnly},
			{UserID: "tok-1", Email: "token-tok1@zerops.io", Status: "ACTIVE", RoleCode: roles.Admin},
		},
		Gitea: applied(t),
	}
	plan := mirror.Compute(state, opts())
	for _, a := range plan.Actions {
		if strings.Contains(a.Login, "tok") {
			t.Errorf("the plan acts on an integration token: %s", a)
		}
	}
	for _, p := range append(plan.Problems, plan.AwaitingSignIn...) {
		if strings.Contains(p, "tok-1") {
			t.Errorf("an integration token was reported as a person: %s", p)
		}
	}
}

func TestOlderTokenGenerationsWaitForTheGrace(t *testing.T) {
	reg, problems := oneGroup(t)

	cases := []struct {
		name       string
		newestAge  time.Duration
		wantDelete []string
	}{
		{name: "the newest is a minute old: nothing goes", newestAge: time.Minute},
		{name: "the newest is nine minutes old: nothing goes", newestAge: 9 * time.Minute},
		{name: "the newest is eleven minutes old: the older ones go", newestAge: 11 * time.Minute,
			wantDelete: []string{"mate/mate-p-fen/1", "mate/mate-p-fen/2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := applied(t)
			g.BotTokens = map[string][]gitea.AccessToken{"mate-p-fen": {
				liveToken(1, "11111111", 3*time.Hour),
				liveToken(2, "22222222", 2*time.Hour),
				liveToken(3, "33333333", tc.newestAge),
				{Name: "some-hand-made-token", CreatedAt: now.Add(-4 * time.Hour)},
			}}
			state := mirror.State{
				Registry: reg, Problems: problems,
				Mates:        map[string]string{"p-fen": "Fen"},
				Members:      settled(),
				Gitea:        g,
				MateServices: served("tok-33333333"),
			}
			plan := mirror.Compute(state, opts())

			var deleted []string
			for _, a := range plan.Actions {
				if a.Kind == mirror.DeleteBotToken {
					deleted = append(deleted, a.TokenName)
				}
			}
			sort.Strings(deleted)
			if strings.Join(deleted, ",") != strings.Join(tc.wantDelete, ",") {
				t.Errorf("deleted = %v, want %v", deleted, tc.wantDelete)
			}
		})
	}
}

func TestOneGenerationIsNeverRevoked(t *testing.T) {
	reg, problems := oneGroup(t)
	g := applied(t)
	g.BotTokens = map[string][]gitea.AccessToken{"mate-p-fen": {liveToken(1, "11111111", 3*time.Hour)}}
	plan := mirror.Compute(mirror.State{
		Registry: reg, Problems: problems, Mates: map[string]string{"p-fen": "Fen"},
		Members: settled(), Gitea: g, MateServices: served("tok-11111111"),
	}, opts())
	for _, a := range plan.Actions {
		if a.Kind == mirror.DeleteBotToken {
			t.Errorf("the only live token was revoked: %s", a)
		}
	}
}

func TestRegistryProblemsAreReportedNotActedOn(t *testing.T) {
	reg, problems := registry.Parse([]string{
		"mate:gn:g-acme:acme",
		"mate:gn:g-other:ACME",         // not a slug
		"mate:gm:g-ghost:p-1:mate",     // no such group
		"mate:gm:g-acme:p-1:something", // not a kind
	})
	plan := mirror.Compute(mirror.State{Registry: reg, Problems: problems, Gitea: emptyGitea()}, opts())

	if len(plan.Problems) < 3 {
		t.Errorf("problems = %v", plan.Problems)
	}
	for _, a := range plan.Actions {
		if a.Org != "" && a.Org != "acme" {
			t.Errorf("the plan acts on an org no good tag named: %s", a)
		}
	}
}

func TestParseTokenName(t *testing.T) {
	cases := []struct {
		bot, name string
		want      int
		ok        bool
	}{
		{"mate-p1", "mate/mate-p1/1", 1, true},
		{"mate-p1", "mate/mate-p1/12", 12, true},
		{"mate-p1", "mate/mate-p2/1", 0, false},
		{"mate-p1", "mate/mate-p1/0", 0, false},
		{"mate-p1", "mate/mate-p1/x", 0, false},
		{"mate-p1", "automation", 0, false},
	}
	for _, tc := range cases {
		got, ok := mirror.ParseTokenName(tc.bot, tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseTokenName(%q, %q) = %d, %v; want %d, %v", tc.bot, tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if got := mirror.TokenName("mate-p1", 3); got != "mate/mate-p1/3" {
		t.Errorf("TokenName = %q", got)
	}
}

// A Mate's Gitea access is delivered by the pass, not asked for: the plan
// carries one action per Mate whose container is short of the three
// variables or whose token is not the bot's newest live generation.
func TestMateAccessIsPlannedWhenItIsNotTrue(t *testing.T) {
	reg, problems := oneGroup(t)

	cases := []struct {
		name     string
		tokens   []gitea.AccessToken
		services map[string]mirror.MateService
		problems map[string]string
		want     bool // an action is planned
		mint     bool // and it mints a new generation
	}{
		{
			name:   "the three variables are right and the token is the newest generation: nothing",
			tokens: []gitea.AccessToken{liveToken(1, "11111111", time.Hour)}, services: served("tok-11111111"),
		},
		{
			name:   "GITEA_TOKEN is absent",
			tokens: []gitea.AccessToken{liveToken(1, "11111111", time.Hour)},
			services: func() map[string]mirror.MateService {
				s := served("tok-11111111")
				delete(s["p-fen"].Vars, mirror.VarGiteaToken)
				return s
			}(),
			want: true, mint: true,
		},
		{
			name:   "GITEA_URL names another Gitea, so the token there is not ours",
			tokens: []gitea.AccessToken{liveToken(1, "11111111", time.Hour)},
			services: func() map[string]mirror.MateService {
				s := served("tok-11111111")
				s["p-fen"].Vars[mirror.VarGiteaURL] = zerops.ServiceUserData{ID: "ud-1", Key: mirror.VarGiteaURL, Content: "https://elsewhere.example"}
				return s
			}(),
			want: true, mint: true,
		},
		{
			name: "the bot has no live generation", tokens: nil, services: served("tok-11111111"),
			want: true, mint: true,
		},
		{
			name:   "the bot's only tokens are hand-made ones",
			tokens: []gitea.AccessToken{{Name: "hand-made", TokenLastEight: "11111111"}}, services: served("tok-11111111"),
			want: true, mint: true,
		},
		{
			name: "the container holds an older generation than the newest — a crash between mint and write",
			tokens: []gitea.AccessToken{
				liveToken(1, "11111111", time.Hour), liveToken(2, "22222222", time.Minute),
			},
			services: served("tok-11111111"),
			want:     true, mint: true,
		},
		{
			name:   "an empty container: every variable is missing",
			tokens: nil, services: map[string]mirror.MateService{"p-fen": {ServiceID: "s-zcp", Vars: map[string]zerops.ServiceUserData{}}},
			want: true, mint: true,
		},
		{
			name:   "only MATE_BROKER_URL is wrong: written, no new generation",
			tokens: []gitea.AccessToken{liveToken(1, "11111111", time.Hour)},
			services: func() map[string]mirror.MateService {
				s := served("tok-11111111")
				s["p-fen"].Vars[mirror.VarBrokerURL] = zerops.ServiceUserData{ID: "ud-2", Key: mirror.VarBrokerURL, Content: "https://old-broker.example"}
				return s
			}(),
			want: true, mint: false,
		},
		{
			name:     "the container could not be read: reported, not acted on",
			tokens:   nil,
			problems: map[string]string{"p-fen": "the broker's token does not reach its project (403)"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := applied(t)
			g.BotTokens = map[string][]gitea.AccessToken{}
			if tc.tokens != nil {
				g.BotTokens["mate-p-fen"] = tc.tokens
			}
			plan := mirror.Compute(mirror.State{
				Registry: reg, Problems: problems, Mates: map[string]string{"p-fen": "Fen"},
				Members: settled(), Gitea: g, MateServices: tc.services, MateProblems: tc.problems,
			}, opts())

			var deliveries []mirror.Action
			for _, a := range plan.Actions {
				if a.Kind == mirror.DeliverMateAccess {
					deliveries = append(deliveries, a)
				}
			}
			if !tc.want {
				if len(deliveries) != 0 {
					t.Fatalf("planned %v", deliveries)
				}
				if len(tc.problems) > 0 && !mentions(plan.Problems, "p-fen") {
					t.Errorf("the unreadable Mate is not reported: %v", plan.Problems)
				}
				return
			}
			if len(deliveries) != 1 {
				t.Fatalf("planned %d deliveries, want one:\n%s", len(deliveries), mirror.Describe(plan))
			}
			a := deliveries[0]
			if a.Org != "acme" || a.Login != "mate-p-fen" || a.Project != "p-fen" || a.Service != "s-zcp" || a.FullName != "Fen" {
				t.Errorf("action = %+v", a)
			}
			if a.Mint != tc.mint {
				t.Errorf("mint = %v, want %v", a.Mint, tc.mint)
			}
			if a.Destructive() {
				t.Error("a delivery counts against the cap")
			}
		})
	}
}

// A Mate the state says nothing about — neither a container nor a reason —
// is reported, never acted on.
func TestAMateWithNoContainerReadIsReported(t *testing.T) {
	reg, problems := oneGroup(t)
	plan := mirror.Compute(mirror.State{
		Registry: reg, Problems: problems, Mates: map[string]string{"p-fen": "Fen"},
		Members: settled(), Gitea: applied(t),
	}, opts())
	for _, a := range plan.Actions {
		if a.Kind == mirror.DeliverMateAccess {
			t.Fatalf("planned %s with no container to write to", a)
		}
	}
	if !mentions(plan.Problems, "p-fen") {
		t.Errorf("problems = %v", plan.Problems)
	}
}

// A pass that mints revokes nothing of that bot: the older generations wait
// for a pass without a mint, so a crash between mint and write never leaves a
// Mate with a dead token and no live one.
func TestARevocationWaitsForAPassWithoutAMint(t *testing.T) {
	reg, problems := oneGroup(t)
	g := applied(t)
	g.BotTokens = map[string][]gitea.AccessToken{"mate-p-fen": {
		liveToken(1, "11111111", 3*time.Hour),
		liveToken(2, "22222222", 11*time.Minute),
	}}
	services := served("tok-11111111")
	delete(services["p-fen"].Vars, mirror.VarGiteaToken)

	plan := mirror.Compute(mirror.State{
		Registry: reg, Problems: problems, Mates: map[string]string{"p-fen": "Fen"},
		Members: settled(), Gitea: g, MateServices: services,
	}, opts())

	var minted, revoked bool
	for _, a := range plan.Actions {
		switch a.Kind {
		case mirror.DeliverMateAccess:
			minted = a.Mint
		case mirror.DeleteBotToken:
			revoked = true
		}
	}
	if !minted {
		t.Fatal("no generation is minted for a container without a token")
	}
	if revoked {
		t.Errorf("a generation was revoked on the same pass as a mint:\n%s", mirror.Describe(plan))
	}
}

func mentions(problems []string, needle string) bool {
	for _, p := range problems {
		if strings.Contains(p, needle) {
			return true
		}
	}
	return false
}

func TestStaleAppTokensAreRetiredAndNeverCounted(t *testing.T) {
	reg, problems := oneGroup(t)
	g := applied(t)
	g.PersonTokens = map[string][]gitea.AccessToken{
		"u-jan": {
			{Name: mirror.AppTokenPrefix + "1", CreatedAt: now.Add(-13 * time.Hour)},
			{Name: mirror.AppTokenPrefix + "2", CreatedAt: now.Add(-11 * time.Hour)},
			{Name: mirror.AppTokenPrefix + "3", CreatedAt: now.Add(-time.Minute)},
			{Name: "hand-made", CreatedAt: now.Add(-40 * time.Hour)},
			{Name: mirror.AppTokenPrefix + "no-clock"},
		},
	}
	state := mirror.State{
		Registry: reg, Problems: problems,
		Mates:        map[string]string{"p-fen": "Fen"},
		Members:      settled(),
		Gitea:        g,
		MateServices: served("tok-33333333"),
	}
	plan := mirror.Compute(state, opts())

	var retired []string
	for _, a := range plan.Actions {
		if a.Kind == mirror.DeletePersonToken {
			retired = append(retired, a.Login+"/"+a.TokenName)
			if a.Destructive() {
				t.Errorf("%s counts against the cap; a retirement by age takes nothing from anybody", a)
			}
		}
	}
	sort.Strings(retired)
	want := []string{"u-jan/" + mirror.AppTokenPrefix + "1"}
	if strings.Join(retired, ",") != strings.Join(want, ",") {
		t.Errorf("retired = %v, want %v: only an app token past the TTL goes; a hand-made one and one with no clock stay", retired, want)
	}
}

// D23: a recipe a Mate proposed is merged by the pass, and nothing else is —
// a person's pull request is theirs, a bot of another group is nobody here, a
// request against another branch or already closed is left alone.
func TestAMatesRecipePullRequestIsMergedAndNobodyElses(t *testing.T) {
	reg, problems := oneGroup(t)
	g := emptyGitea()
	main := gitea.PullRequestBranch{Ref: "main"}
	g.GroupPullRequests = map[string][]gitea.PullRequest{"acme": {
		{Number: 1, State: "open", User: gitea.PullRequestUser{Login: "mate-p-fen"}, Base: main},
		{Number: 2, State: "open", User: gitea.PullRequestUser{Login: roles.Login("u-jan")}, Base: main},
		{Number: 3, State: "open", User: gitea.PullRequestUser{Login: "mate-p-elsewhere"}, Base: main},
		{Number: 4, State: "open", User: gitea.PullRequestUser{Login: "mate-p-fen"}, Base: gitea.PullRequestBranch{Ref: "env/stage"}},
		{Number: 5, State: "closed", Merged: true, User: gitea.PullRequestUser{Login: "mate-p-fen"}, Base: main},
	}}
	plan := mirror.Compute(mirror.State{Registry: reg, Problems: problems, Gitea: g}, opts())

	var merged []int64
	for _, a := range plan.Actions {
		if a.Kind == mirror.MergeRecipePullRequest {
			merged = append(merged, a.PullRequest)
		}
	}
	if len(merged) != 1 || merged[0] != 1 {
		t.Fatalf("merged = %v, want #1 alone:\n%s", merged, mirror.Describe(plan))
	}
	if plan.Destructive() != 0 {
		t.Errorf("a merge counted as destructive")
	}
}
