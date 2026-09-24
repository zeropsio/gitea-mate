// Package mirror is the rights loop of guide 1.4: Zerops roles in, Gitea
// writes out.
//
// It is two halves on purpose. [Compute] is pure — a state in, a list of writes
// out — so every rule is a table test; [Mirror.Apply] performs them in order.
// Between the two sits the cap: a group whose plan would disable, remove or
// delete more than a configured number of things is held and reported instead
// of applied, and so is the whole pass when that many belong to no group.
//
// The read side has its own rule, and it is the important one: a member list,
// project list or registry read that fails or comes back partial ends the pass
// with no write at all. A truncated list must never read as a shrunken org.
package mirror

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
)

// The three teams every group org carries (docs/vocabulary.md).
const (
	TeamRead    = "read"
	TeamWrite   = "write"
	TeamRelease = "release"
)

// HookEvents are the events the broker's one org hook subscribes to.
var HookEvents = []string{"push", "create", "delete", "pull_request", "workflow_job", "workflow_run"}

// BotScopes is what a Mate bot's token may do. GET /user needs read:user —
// write:repository alone is 403 (measured on Gitea 1.27.2).
var BotScopes = []string{"write:repository", "read:user"}

// AppTokenPrefix names the tokens the broker mints for a person's app
// session (POST /person/token): mate-app/{unix nanoseconds}. Gitea refuses a
// name a user already holds, so the stamp keeps every mint distinct.
const AppTokenPrefix = "mate-app/"

// DefaultAppTokenTTL is how long a person's app token lives. A tab rarely
// outlives it, and a copied value dies with it.
const DefaultAppTokenTTL = 12 * time.Hour

// DefaultTokenGrace is how long a superseded generation of a bot's token
// lives before the loop revokes it.
const DefaultTokenGrace = 10 * time.Minute

// BotLogin is a Mate's bot user: mate-{projectId} (docs/vocabulary.md).
func BotLogin(projectID string) string { return "mate-" + projectID }

// TokenName is a bot token's name: mate/{bot}/{generation}. The generation
// lives in the name; the broker keeps no store.
func TokenName(bot string, generation int) string {
	return "mate/" + bot + "/" + strconv.Itoa(generation)
}

// ParseTokenName reads a generation out of a bot token's name.
func ParseTokenName(bot, name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "mate/"+bot+"/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// State is everything a pass reads. Gathering it is [Mirror.Gather]'s job, so
// the planner never touches a network.
type State struct {
	// Members is the org's member list, integration-token pseudo-members
	// included — the planner drops those itself.
	Members []Member
	// Overrides is each person's per-project role: user id -> project id ->
	// role, from every project's userRoles.
	Overrides map[string]map[string]roles.Role
	// Registry is the parsed registry, and Problems what it could not take.
	Registry registry.Registry
	Problems []registry.Problem
	// Gitea is the current Gitea state.
	Gitea GiteaState
	// Mates names each Mate project, so a bot can carry its Mate's name.
	Mates map[string]string
	// MateServices is each Mate's zcp container, by project id, for the Mates
	// whose project the pass could read; MateProblems says, for the others,
	// why not. A Mate in neither is one the state was built without.
	MateServices map[string]MateService
	MateProblems map[string]string
	// DeadMates are the registered Mates whose project is deleted in Zerops.
	// Their entries are out of Registry; their bots are retired.
	DeadMates []string
}

// Member is one person in the org, as the planner needs them.
type Member struct {
	UserID            string
	Email             string
	FullName          string
	Status            string
	RoleCode          roles.Role
	CanCreateProjects bool
}

// IsToken reports whether this row is an integration token's pseudo-member
// rather than a person. Every integration token appears in the member list as
// token-<id>@zerops.io (ledger 2026-09-15), and no Gitea account is ever made
// for one.
func (m Member) IsToken() bool {
	return strings.HasPrefix(m.Email, "token-") && strings.HasSuffix(m.Email, "@zerops.io")
}

// GiteaState is what Gitea currently holds.
type GiteaState struct {
	// Users is every Gitea account, by login.
	Users map[string]gitea.User
	// Orgs is the orgs that exist, by name.
	Orgs map[string]bool
	// Teams is org -> team name -> team.
	Teams map[string]map[string]TeamState
	// Repos is org -> repo name -> state.
	Repos map[string]map[string]RepoState
	// Hooks is org -> its hooks.
	Hooks map[string][]gitea.Hook
	// BotTokens is bot login -> its tokens.
	BotTokens map[string][]gitea.AccessToken
	// PersonTokens is person login -> its tokens, read for every account the
	// broker made a person's app token for (AppTokenPrefix).
	PersonTokens map[string][]gitea.AccessToken
	// GroupPullRequests is org -> the pull requests open on its group repo.
	GroupPullRequests map[string][]gitea.PullRequest
}

// TeamState is one team and who is in it.
type TeamState struct {
	ID      int64
	Members map[string]bool
}

// RepoState is one repository's protections.
type RepoState struct {
	BranchRules map[string]gitea.BranchProtection
	TagRules    map[string]gitea.TagProtection
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// Kind is what an action does.
type Kind string

const (
	CreateOrg          Kind = "create_org"
	CreateTeam         Kind = "create_team"
	CreateRepo         Kind = "create_repo"
	SetBranchRule      Kind = "set_branch_rule"
	SetTagRule         Kind = "set_tag_rule"
	CreateHook         Kind = "create_hook"
	CreateBot          Kind = "create_bot"
	ShapeBot           Kind = "shape_bot"
	AddTeamMember      Kind = "add_team_member"
	RemoveTeamMember   Kind = "remove_team_member"
	PromoteSiteAdmin   Kind = "promote_site_admin"
	DemoteSiteAdmin    Kind = "demote_site_admin"
	DeactivatePerson   Kind = "deactivate_person"
	DeletePersonTokens Kind = "delete_person_tokens"
	// DeleteBotTokens deletes a bot's superseded generations, all of them in
	// one action, so the cap counts one bot's cleanup once however long its
	// pile is.
	DeleteBotTokens Kind = "delete_bot_tokens"
	// DeletePersonToken retires one of a person's app tokens by age. The
	// app re-mints silently on the 401 that follows, so this takes nothing
	// from anybody and the cap does not count it.
	DeletePersonToken Kind = "delete_person_token"
	// MergeRecipePullRequest lands a recipe a Mate proposed on its group repo
	// (D23): a merge, nothing taken away.
	MergeRecipePullRequest Kind = "merge_recipe_pull_request"
	DeliverMateAccess      Kind = "deliver_mate_access"
	// RetireBot deletes every token of a deleted Mate's bot and prohibits its
	// login. The platform's projectNotFound is the guard, so the cap does not
	// count it.
	RetireBot Kind = "retire_bot"
)

// Action is one Gitea write. Only the fields its Kind needs are set.
type Action struct {
	Kind Kind

	Org   string
	Repo  string
	Team  string
	Login string

	FullName  string
	TokenName string
	// TokenNames are the generations a DeleteBotTokens deletes.
	TokenNames []string

	BranchRule *gitea.BranchProtection
	TagRule    *gitea.TagProtection
	HookURL    string

	// Project and Service name the Mate and its container a delivery writes
	// to; Mint says whether it mints a new token generation first.
	Project string
	Service string
	Mint    bool

	// PullRequest is the number a MergeRecipePullRequest merges.
	PullRequest int64
}

// Destructive reports whether this action takes something away. The cap counts
// exactly these. One bot's cleanup of its superseded generations is one, in
// its group, however many generations it deletes: counted per generation, a
// pile longer than the cap would hold the group on every pass.
func (a Action) Destructive() bool {
	switch a.Kind {
	case RemoveTeamMember, DemoteSiteAdmin, DeactivatePerson, DeletePersonTokens, DeleteBotTokens:
		return true
	}
	return false
}

// String is the log line. It never names a token value — only its name.
func (a Action) String() string {
	var b strings.Builder
	b.WriteString(string(a.Kind))
	for _, f := range []struct{ k, v string }{
		{"org", a.Org}, {"repo", a.Repo}, {"team", a.Team},
		{"login", a.Login}, {"token", a.TokenName}, {"tokens", strings.Join(a.TokenNames, ",")},
		{"project", a.Project}, {"service", a.Service},
	} {
		if f.v != "" {
			fmt.Fprintf(&b, " %s=%s", f.k, f.v)
		}
	}
	if a.BranchRule != nil {
		fmt.Fprintf(&b, " rule=%s", a.BranchRule.RuleName)
	}
	if a.PullRequest != 0 {
		fmt.Fprintf(&b, " pr=#%d", a.PullRequest)
	}
	if a.TagRule != nil {
		fmt.Fprintf(&b, " tags=%s", a.TagRule.NamePattern)
	}
	if a.Mint {
		b.WriteString(" mint=true")
	}
	return b.String()
}

// Plan is a pass's intent.
type Plan struct {
	Actions []Action
	// Problems are things the pass noticed and will not act on, and that a
	// person has to put right: a malformed registry tag, a group whose org
	// could not be made.
	Problems []string
	// AwaitingSignIn are the people who hold rights and have no Gitea account
	// yet. Gitea makes an account at a person's first sign-in, so this is the
	// ordinary state of everyone who has not signed in: the pass counts them
	// and stays problem-free. Somebody who has an account and is missing from
	// a team is a diff, and lands in Actions.
	AwaitingSignIn []string
}

// Destructive counts the actions the cap governs.
func (p Plan) Destructive() int {
	n := 0
	for _, a := range p.Actions {
		if a.Destructive() {
			n++
		}
	}
	return n
}

// Options are the planner's knobs.
type Options struct {
	// AdminLogin is the site admin the broker itself is. It is never demoted,
	// never deactivated, and it is the only writer of env/*.
	AdminLogin string
	// HookURL is where Gitea posts this account's webhooks.
	HookURL string
	// GiteaPublicURL and BrokerPublicURL are what a Mate's container must
	// hold as GITEA_URL and MATE_BROKER_URL.
	GiteaPublicURL  string
	BrokerPublicURL string
	// Now is the clock the token grace is measured against.
	Now time.Time
	// TokenGrace defaults to DefaultTokenGrace.
	TokenGrace time.Duration
	// AppTokenTTL is how long a person's app token lives before a pass
	// retires it; defaults to DefaultAppTokenTTL.
	AppTokenTTL time.Duration
}

// Compute works out every Gitea write the current state asks for, in an order
// Gitea accepts: an org before its teams, its teams before the group
// repository's rules (a protection naming a team that does not exist is 422 —
// measured on 1.27.2), and people after the teams they join.
func Compute(state State, opts Options) Plan {
	if opts.TokenGrace <= 0 {
		opts.TokenGrace = DefaultTokenGrace
	}
	if opts.AppTokenTTL <= 0 {
		opts.AppTokenTTL = DefaultAppTokenTTL
	}
	p := &planner{state: state, opts: opts}
	p.plan()
	return Plan{Actions: p.actions, Problems: p.problems, AwaitingSignIn: p.awaiting}
}

type planner struct {
	state    State
	opts     Options
	actions  []Action
	problems []string
	awaiting []string
}

func (p *planner) do(a Action)             { p.actions = append(p.actions, a) }
func (p *planner) note(f string, a ...any) { p.problems = append(p.problems, fmt.Sprintf(f, a...)) }

func (p *planner) plan() {
	for _, problem := range p.state.Problems {
		p.note("registry tag %s", problem)
	}

	// The order is what Gitea will accept: structure, then the bots, then the
	// people and the bots into their teams (a team member who does not exist
	// yet is a 404), then departures, then each Mate's access, then token
	// generations.
	p.planStructure()
	p.planRecipePullRequests()
	p.planBots()
	p.planPeople()
	p.planDepartures()
	p.planMateAccess()
	p.planBotTokens()
	p.planAppTokens()
	p.planRetirements()
}

// planStructure: an org, its three teams, its group repository and its
// protections, and the one webhook — per registered group.
func (p *planner) planStructure() {
	for _, g := range p.state.Registry.Groups {
		if !p.state.Gitea.Orgs[g.Slug] {
			p.do(Action{Kind: CreateOrg, Org: g.Slug, FullName: g.Slug})
		}
		teams := p.state.Gitea.Teams[g.Slug]
		for _, t := range []struct{ name, permission string }{
			{TeamRead, "read"},
			{TeamWrite, "write"},
			// The releasers write too; what sets them apart is the v* tag
			// protection, which has no admin override.
			{TeamRelease, "write"},
		} {
			if _, exists := teams[t.name]; !exists {
				p.do(Action{Kind: CreateTeam, Org: g.Slug, Team: t.name, FullName: t.permission})
			}
		}

		repos := p.state.Gitea.Repos[g.Slug]
		repo, hasRepo := repos[registry.GroupRepo]
		if !hasRepo {
			p.do(Action{Kind: CreateRepo, Org: g.Slug, Repo: registry.GroupRepo})
		}
		for _, want := range p.groupRepoRules() {
			if hasRepo {
				if have, ok := repo.BranchRules[want.RuleName]; ok && sameBranchRule(have, want) {
					continue
				}
			}
			rule := want
			p.do(Action{Kind: SetBranchRule, Org: g.Slug, Repo: registry.GroupRepo, BranchRule: &rule})
		}
		wantTag := gitea.TagProtection{NamePattern: "v*", WhitelistTeams: []string{TeamRelease}}
		if !hasRepo || !sameTagRule(repo.TagRules[wantTag.NamePattern], wantTag) {
			rule := wantTag
			p.do(Action{Kind: SetTagRule, Org: g.Slug, Repo: registry.GroupRepo, TagRule: &rule})
		}

		if !p.hasHook(g.Slug) {
			p.do(Action{Kind: CreateHook, Org: g.Slug, HookURL: p.opts.HookURL})
		}
	}
}

// groupRepoRules is what protects a group repository: main takes no direct
// push from anyone and merges from anyone with write — the write and release
// teams, and the broker landing a Mate's proposal (D23; until 2026-09-17 the
// release team alone, which left every Mate's recipe waiting for a releaser);
// env/* is the broker's alone, named by rule so it holds before the branch
// exists. What sets the releasers apart is the v* tag protection.
func (p *planner) groupRepoRules() []gitea.BranchProtection {
	return []gitea.BranchProtection{
		{
			RuleName:   groupMainBranch,
			EnablePush: false,
		},
		{
			RuleName:            "env/*",
			EnablePush:          true,
			EnablePushWhitelist: true,
			PushWhitelistUsers:  []string{p.opts.AdminLogin},
		},
	}
}

// groupMainBranch is the group repo's protected branch, where the recipe and
// the declarations live.
const groupMainBranch = "main"

// planRecipePullRequests lands every recipe a Mate proposed (D23): a pull
// request open on the group repo against main, opened by the bot of a Mate
// registered in that group, is merged as it is. The group repo takes merges
// from anyone with write, and a Mate's proposal is the group's own import,
// not something a person has to review — the owner, 2026-09-17, on a first
// recipe that waited for a releaser: "they all should be able to merge on the
// import yaml repo". A person's pull request is theirs to merge; a bot of
// another group is nobody here.
func (p *planner) planRecipePullRequests() {
	for _, g := range p.state.Registry.Groups {
		bots := map[string]bool{}
		for _, prj := range g.Projects {
			if prj.Kind == roles.KindMate {
				bots[BotLogin(prj.ID)] = true
			}
		}
		for _, pr := range p.state.Gitea.GroupPullRequests[g.Slug] {
			if pr.State != "open" || pr.Merged || pr.Base.Ref != groupMainBranch || !bots[pr.User.Login] {
				continue
			}
			p.do(Action{Kind: MergeRecipePullRequest, Org: g.Slug, Repo: registry.GroupRepo, Login: pr.User.Login, PullRequest: pr.Number})
		}
	}
}

func (p *planner) hasHook(org string) bool {
	for _, h := range p.state.Gitea.Hooks[org] {
		if h.Config["url"] == p.opts.HookURL {
			return true
		}
	}
	return false
}

// planPeople: each active person's team membership and site-admin flag.
func (p *planner) planPeople() {
	registryRoles := p.state.Registry.Roles()

	// desired[org][team] is who should be in it, so a pass can also see who
	// should not.
	desired := map[string]map[string]map[string]bool{}
	for _, g := range p.state.Registry.Groups {
		desired[g.Slug] = map[string]map[string]bool{
			TeamRead: {}, TeamWrite: {}, TeamRelease: {},
		}
	}

	for _, m := range p.sortedMembers() {
		if m.IsToken() {
			continue
		}
		login := roles.Login(m.UserID)
		rights := roles.Compute(roles.Person{
			ID:                m.UserID,
			OrgRole:           m.RoleCode,
			Status:            m.Status,
			CanCreateProjects: m.CanCreateProjects,
		}, p.state.Overrides[m.UserID], registryRoles)

		user, known := p.state.Gitea.Users[login]
		if !known {
			// A person who has never signed in has no Gitea account: the OIDC
			// source makes one at their first sign-in, and the pass a few
			// seconds later puts them in their teams. Nothing to do, and
			// nothing wrong — waiting for a sign-in is counted, never
			// reported, and an inactive person is not even that.
			if rights.Active && len(rights.Claims) > 0 {
				p.awaiting = append(p.awaiting, login)
			}
			continue
		}

		for _, g := range p.state.Registry.Groups {
			for team, on := range map[string]bool{
				TeamRead:    rights.Groups[g.Slug].Read,
				TeamWrite:   rights.Groups[g.Slug].Write,
				TeamRelease: rights.Groups[g.Slug].Release,
			} {
				if on {
					desired[g.Slug][team][login] = true
				}
			}
		}

		if login == p.opts.AdminLogin {
			continue
		}
		switch {
		case rights.SiteAdmin && !user.IsAdmin:
			p.do(Action{Kind: PromoteSiteAdmin, Login: login})
		case !rights.SiteAdmin && user.IsAdmin:
			p.do(Action{Kind: DemoteSiteAdmin, Login: login})
		}
	}

	// Bots belong to the read team of their group and stay there whatever the
	// people plan says.
	for _, g := range p.state.Registry.Groups {
		for _, prj := range g.Projects {
			if prj.Kind == roles.KindMate {
				desired[g.Slug][TeamRead][BotLogin(prj.ID)] = true
			}
		}
	}

	// A deleted Mate's bot is retired, not moved: taking it out of its team
	// would be a removal counted against the group for a bot that can no
	// longer sign in.
	retired := map[string]bool{}
	for _, projectID := range p.state.DeadMates {
		retired[BotLogin(projectID)] = true
	}
	for _, g := range p.state.Registry.Groups {
		for _, team := range []string{TeamRead, TeamWrite, TeamRelease} {
			current := p.state.Gitea.Teams[g.Slug][team].Members
			for _, login := range sortedKeys(desired[g.Slug][team]) {
				if !current[login] {
					p.do(Action{Kind: AddTeamMember, Org: g.Slug, Team: team, Login: login})
				}
			}
			for _, login := range sortedKeys(current) {
				if desired[g.Slug][team][login] {
					continue
				}
				if login == p.opts.AdminLogin || retired[login] {
					continue
				}
				p.do(Action{Kind: RemoveTeamMember, Org: g.Slug, Team: team, Login: login})
			}
		}
	}
}

// planBots: one bot per Mate project, restricted, creating nothing.
func (p *planner) planBots() {
	for _, g := range p.state.Registry.Groups {
		for _, prj := range g.Projects {
			if prj.Kind != roles.KindMate {
				continue
			}
			login := BotLogin(prj.ID)
			name := p.state.Mates[prj.ID]
			user, exists := p.state.Gitea.Users[login]
			if !exists {
				p.do(Action{Kind: CreateBot, Org: g.Slug, Login: login, FullName: name})
				p.do(Action{Kind: ShapeBot, Org: g.Slug, Login: login, FullName: name})
				continue
			}
			if !user.Restricted || !user.Active {
				p.do(Action{Kind: ShapeBot, Org: g.Slug, Login: login, FullName: name})
			}
		}
	}
}

// planDepartures: a Gitea person who is no longer an active member of the org
// is switched off and their tokens deleted. Their account stays, so their
// history keeps a name.
func (p *planner) planDepartures() {
	active := map[string]bool{}
	for _, m := range p.state.Members {
		if m.IsToken() || m.Status != roles.StatusActive {
			continue
		}
		active[roles.Login(m.UserID)] = true
	}

	for _, login := range sortedUserKeys(p.state.Gitea.Users) {
		user := p.state.Gitea.Users[login]
		if login == p.opts.AdminLogin || !strings.HasPrefix(login, "u-") || active[login] {
			continue
		}
		if user.Active {
			p.do(Action{Kind: DeactivatePerson, Login: login})
			p.do(Action{Kind: DeletePersonTokens, Login: login})
		}
	}
}

// planBotTokens: a bot's superseded generations go, except the newest and the
// one whose tail the container's GITEA_TOKEN ends in (the held one). A
// generation older than the held one goes once the held one — what replaced it
// in the container — is past the grace; one newer than the held one was never
// written and nobody uses it, so it, like any generation of a container that
// holds none, goes once its own successor is past the grace. The clock never
// starts at a successor minted but never written (an ambiguous write, a broker
// down), so a Mate that left a generation keeps it for the full rollover
// window, while a pile of old generations still goes in one pass. That holds on
// a pass that mints for the bot too, so a pile minted while a container refused
// its writes never grows again, and a crash between mint and write still
// leaves the container the token it holds. A needed creation time that is
// missing blocks the deletion and is reported. A bot whose container the pass
// could not read loses nothing: what it holds is unknown.
func (p *planner) planBotTokens() {
	groupOf := map[string]string{}
	projectOf := map[string]string{}
	for _, g := range p.state.Registry.Groups {
		for _, prj := range g.Projects {
			if prj.Kind == roles.KindMate {
				groupOf[BotLogin(prj.ID)] = g.Slug
				projectOf[BotLogin(prj.ID)] = prj.ID
			}
		}
	}
	for _, bot := range sortedTokenKeys(p.state.Gitea.BotTokens) {
		svc, read := p.state.MateServices[projectOf[bot]]
		if !read {
			continue
		}
		value := svc.Vars[VarGiteaToken].Content
		type gen struct {
			n     int
			token gitea.AccessToken
		}
		var gens []gen
		for _, t := range p.state.Gitea.BotTokens[bot] {
			if n, ok := ParseTokenName(bot, t.Name); ok {
				gens = append(gens, gen{n: n, token: t})
			}
		}
		sort.Slice(gens, func(i, j int) bool { return gens[i].n > gens[j].n })
		held := -1
		for i, g := range gens {
			if value != "" && g.token.TokenLastEight != "" && strings.HasSuffix(value, g.token.TokenLastEight) {
				held = i
				break
			}
		}
		undated := map[int]bool{}
		pastGrace := func(g gen) bool {
			if g.token.CreatedAt.IsZero() {
				if !undated[g.n] {
					undated[g.n] = true
					p.note("bot %s: generation %d has no creation time; nothing older revoked", bot, g.n)
				}
				return false
			}
			return p.opts.Now.Sub(g.token.CreatedAt) >= p.opts.TokenGrace
		}
		var names []string
		for i := 1; i < len(gens); i++ {
			if i == held {
				continue
			}
			replacedBy := gens[i-1]
			if held >= 0 && i > held {
				replacedBy = gens[held]
			}
			if pastGrace(replacedBy) {
				names = append(names, gens[i].token.Name)
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		p.do(Action{Kind: DeleteBotTokens, Org: groupOf[bot], Login: bot, TokenNames: names})
	}
}

// planRetirements: the bot of a Mate whose project is deleted is retired once;
// a bot already prohibited from signing in is left alone.
func (p *planner) planRetirements() {
	dead := append([]string(nil), p.state.DeadMates...)
	sort.Strings(dead)
	for _, projectID := range dead {
		login := BotLogin(projectID)
		if bot, exists := p.state.Gitea.Users[login]; exists && !bot.ProhibitLogin {
			p.do(Action{Kind: RetireBot, Login: login, Project: projectID})
		}
	}
}

// planAppTokens: a person's app tokens (AppTokenPrefix, minted by
// POST /person/token) carry no expiry of their own — Gitea has none — so the
// pass retires every one older than the TTL. The app holds the value in
// memory for a tab's life and re-mints on the first 401, so a retirement is
// never noticed; it only bounds how long a copied value stays good.
func (p *planner) planAppTokens() {
	for _, login := range sortedTokenKeys(p.state.Gitea.PersonTokens) {
		for _, t := range p.state.Gitea.PersonTokens[login] {
			if !strings.HasPrefix(t.Name, AppTokenPrefix) || t.CreatedAt.IsZero() {
				continue
			}
			if p.opts.Now.Sub(t.CreatedAt) >= p.opts.AppTokenTTL {
				p.do(Action{Kind: DeletePersonToken, Login: login, TokenName: t.Name})
			}
		}
	}
}

func (p *planner) sortedMembers() []Member {
	out := append([]Member(nil), p.state.Members...)
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

func sameBranchRule(have, want gitea.BranchProtection) bool {
	if have.EnablePush != want.EnablePush ||
		have.EnablePushWhitelist != want.EnablePushWhitelist ||
		have.EnableMergeWhitelist != want.EnableMergeWhitelist ||
		have.BlockAdminMergeOverride != want.BlockAdminMergeOverride {
		return false
	}
	// A list behind a switch that is off is not in force. Gitea keeps the old
	// names on it, and they must not make a rule read as different on every
	// pass (main's release list after D23 turned its merge whitelist off).
	if want.EnablePushWhitelist &&
		(!sameStrings(have.PushWhitelistTeams, want.PushWhitelistTeams) ||
			!sameStrings(have.PushWhitelistUsers, want.PushWhitelistUsers)) {
		return false
	}
	if want.EnableMergeWhitelist &&
		(!sameStrings(have.MergeWhitelistTeams, want.MergeWhitelistTeams) ||
			!sameStrings(have.MergeWhitelistUsers, want.MergeWhitelistUsers)) {
		return false
	}
	return true
}

func sameTagRule(have, want gitea.TagProtection) bool {
	return have.NamePattern == want.NamePattern && sameStrings(have.WhitelistTeams, want.WhitelistTeams)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUserKeys(m map[string]gitea.User) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTokenKeys(m map[string][]gitea.AccessToken) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
