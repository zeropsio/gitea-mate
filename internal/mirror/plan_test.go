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
)

const (
	adminLogin = "admin"
	hookURL    = "https://broker.example/hooks/gitea"
)

var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

func opts() mirror.Options {
	return mirror.Options{AdminLogin: adminLogin, HookURL: hookURL, Now: now}
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
	if !main.EnableMergeWhitelist || len(main.MergeWhitelistTeams) != 1 || main.MergeWhitelistTeams[0] != "release" {
		t.Errorf("main merges = %+v; the group repo takes merges from the release team", main.MergeWhitelistTeams)
	}
	if !main.BlockAdminMergeOverride {
		t.Error("an admin can still override the merge on main")
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
		Gitea: applied(t),
	}
	plan := mirror.Compute(state, opts())
	if len(plan.Actions) != 0 {
		t.Errorf("a second pass plans %d actions:\n%s", len(plan.Actions), mirror.Describe(plan))
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
			"main": {
				RuleName: "main", EnablePush: false, EnableMergeWhitelist: true,
				MergeWhitelistTeams: []string{"release"}, BlockAdminMergeOverride: true,
			},
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
	for _, p := range plan.Problems {
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
				{Name: "mate/mate-p-fen/1", CreatedAt: now.Add(-3 * time.Hour)},
				{Name: "mate/mate-p-fen/2", CreatedAt: now.Add(-2 * time.Hour)},
				{Name: "mate/mate-p-fen/3", CreatedAt: now.Add(-tc.newestAge)},
				{Name: "some-hand-made-token", CreatedAt: now.Add(-4 * time.Hour)},
			}}
			state := mirror.State{
				Registry: reg, Problems: problems,
				Mates:   map[string]string{"p-fen": "Fen"},
				Members: settled(),
				Gitea:   g,
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
	g.BotTokens = map[string][]gitea.AccessToken{"mate-p-fen": {
		{Name: "mate/mate-p-fen/1", CreatedAt: now.Add(-3 * time.Hour)},
	}}
	plan := mirror.Compute(mirror.State{
		Registry: reg, Problems: problems, Mates: map[string]string{"p-fen": "Fen"},
		Members: settled(), Gitea: g,
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
