package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// ErrCapped is returned by a pass whose plan would take away more than the cap
// allows. Nothing is written; the plan is in the result for a person to read.
var ErrCapped = errors.New("the plan exceeds the destructive cap")

// ErrUnreadable is returned when a pass could not read the org. Nothing is
// written: a member list, project list or registry read that fails or comes
// back partial ends the pass where it stands.
var ErrUnreadable = errors.New("the org could not be read")

// Mirror runs passes.
type Mirror struct {
	Zerops *zerops.Client
	Gitea  *gitea.Client
	Log    *slog.Logger

	// ClientID is the Zerops org; GiteaProjectID the project whose tags are
	// the registry.
	ClientID       string
	GiteaProjectID string

	// AdminLogin is the site admin the broker is.
	AdminLogin string
	// AppOrigins is every origin the Mate app runs from. The app's public
	// OAuth2 client is registered for the callback of each.
	AppOrigins []string
	// HookURL is where Gitea posts this account's webhooks.
	HookURL string
	// GiteaPublicURL and BrokerPublicURL are what every Mate's container is
	// given as GITEA_URL and MATE_BROKER_URL.
	GiteaPublicURL  string
	BrokerPublicURL string
	// Cap is the most destructive actions one pass may apply.
	Cap int
	// TokenGrace overrides DefaultTokenGrace.
	TokenGrace time.Duration
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time

	// hookSecret is the HMAC secret the broker puts on the hooks it creates.
	// It is unexported and write-only (SetHookSecret) so no call site can pass
	// it by accident into something that logs.
	hookSecret string

	// mu guards appClient, which a pass writes and the /gitea/oauth-client
	// route reads.
	mu        sync.Mutex
	appClient *AppClient
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mirror) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Result is what a pass did. It is counts, never names of tokens.
type Result struct {
	Planned     int
	Applied     int
	Destructive int
	Problems    []string
	Failures    []string
	// AwaitingSignIn is how many people hold rights and have no Gitea account
	// yet. It is information, not a problem: the account is made at the first
	// sign-in.
	AwaitingSignIn int
	Plan           Plan
}

// LogValue is what the loop logs: counts and nothing that identifies a
// credential.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("planned", r.Planned),
		slog.Int("applied", r.Applied),
		slog.Int("destructive", r.Destructive),
		slog.Int("problems", len(r.Problems)),
		slog.Int("failures", len(r.Failures)),
		slog.Int("awaiting_sign_in", r.AwaitingSignIn),
	)
}

// Pass reads the org, plans and applies. It is the whole loop.
func (m *Mirror) Pass(ctx context.Context) (Result, error) {
	// The Mate app's OAuth2 client depends on nothing the org says, so it is
	// made true first: a Zerops that cannot be read must not leave the app
	// without the client id it signs people in with.
	var appFailure string
	if err := m.EnsureAppClient(ctx); err != nil {
		appFailure = "the Mate app's OAuth2 client: " + err.Error()
		m.log().Warn("the Mate app's OAuth2 client could not be registered", "err", err.Error())
	}

	state, err := m.Gather(ctx)
	if err != nil {
		return Result{}, err
	}
	plan := Compute(state, Options{
		AdminLogin:      m.AdminLogin,
		HookURL:         m.HookURL,
		GiteaPublicURL:  m.GiteaPublicURL,
		BrokerPublicURL: m.BrokerPublicURL,
		Now:             m.now(),
		TokenGrace:      m.TokenGrace,
	})
	// What the pass noticed and will not act on is said once per pass, so a
	// Mate the broker cannot serve yet is in the log by name. Never a token.
	for _, problem := range plan.Problems {
		m.log().Warn("the rights loop cannot act on this", "problem", problem)
	}

	result := Result{
		Planned:        len(plan.Actions),
		Destructive:    plan.Destructive(),
		Problems:       plan.Problems,
		AwaitingSignIn: len(plan.AwaitingSignIn),
		Plan:           plan,
	}
	cap := m.Cap
	if cap <= 0 {
		cap = 10
	}
	if result.Destructive > cap {
		return result, fmt.Errorf("%w: %d actions take something away, the cap is %d", ErrCapped, result.Destructive, cap)
	}

	applied, failures := m.Apply(ctx, plan)
	result.Applied = applied
	result.Failures = failures
	if appFailure != "" {
		result.Failures = append(result.Failures, appFailure)
	}
	return result, nil
}

// Gather reads everything a pass needs. Any read that fails, or a project
// search short of its declared total, ends the pass here — before a single
// write.
func (m *Mirror) Gather(ctx context.Context) (State, error) {
	state, err := ReadOrg(ctx, m.Zerops, m.ClientID, m.GiteaProjectID)
	if err != nil {
		return State{}, err
	}
	giteaState, err := m.gatherGitea(ctx, state.Registry)
	if err != nil {
		return State{}, fmt.Errorf("%w: Gitea: %w", ErrUnreadable, err)
	}
	state.Gitea = giteaState
	// A Mate's container that cannot be read is that Mate's problem, not the
	// pass's: it is reported, skipped and read again next time.
	state.MateServices, state.MateProblems = m.gatherMates(ctx, state.Registry)
	return state, nil
}

// ReadOrg reads the Zerops half of a pass: who the org's people are, what they
// hold on each project, and the registry the Gitea project's tags carry. The
// endpoints that must answer "may this person?" read the same thing, so one
// rule is applied everywhere.
//
// Every failure is an ErrUnreadable, including a project search that came back
// a page short of its declared total: a truncated list must never read as a
// shrunken org.
func ReadOrg(ctx context.Context, z *zerops.Client, clientID, giteaProjectID string) (State, error) {
	members, err := z.Members(ctx, clientID)
	if err != nil {
		return State{}, fmt.Errorf("%w: the member list: %w", ErrUnreadable, err)
	}
	if len(members.Members) == 0 {
		return State{}, fmt.Errorf("%w: the member list is empty, which no live org is", ErrUnreadable)
	}

	projects, err := z.SearchProjects(ctx, clientID)
	if err != nil {
		return State{}, fmt.Errorf("%w: the project list: %w", ErrUnreadable, err)
	}

	var giteaProject *zerops.Project
	for i := range projects.Projects {
		if projects.Projects[i].ID == giteaProjectID {
			giteaProject = &projects.Projects[i]
		}
	}
	if giteaProject == nil {
		return State{}, fmt.Errorf("%w: the registry lives on project %s, which the project list does not carry", ErrUnreadable, giteaProjectID)
	}
	reg, problems := registry.Parse(giteaProject.TagList)

	state := State{
		Registry:  reg,
		Problems:  problems,
		Overrides: map[string]map[string]roles.Role{},
		Mates:     map[string]string{},
	}

	// clientUserId -> Zerops user id, so a project's userRoles can name people.
	byClientUser := map[string]string{}
	for _, row := range members.Members {
		byClientUser[row.ID] = row.UserID
		state.Members = append(state.Members, Member{
			UserID:            row.UserID,
			Email:             row.User.Email,
			FullName:          row.User.FullName,
			Status:            row.Status,
			RoleCode:          roles.Role(row.RoleCode),
			CanCreateProjects: row.CanCreateProjects,
		})
	}

	for _, prj := range projects.Projects {
		for _, override := range prj.UserRoles {
			userID, known := byClientUser[override.ClientUserID]
			if !known {
				continue
			}
			if state.Overrides[userID] == nil {
				state.Overrides[userID] = map[string]roles.Role{}
			}
			state.Overrides[userID][prj.ID] = roles.Role(override.RoleCode)
		}
		state.Mates[prj.ID] = prj.Name
	}
	return state, nil
}

// Person finds one member of the org by their Zerops user id.
func (s State) Person(userID string) (Member, bool) {
	for _, m := range s.Members {
		if m.UserID == userID {
			return m, true
		}
	}
	return Member{}, false
}

// RightsFor runs the role function for one person against this state.
func (s State) RightsFor(userID string) (roles.Rights, bool) {
	m, ok := s.Person(userID)
	if !ok {
		return roles.Rights{}, false
	}
	return roles.Compute(roles.Person{
		ID:                m.UserID,
		OrgRole:           m.RoleCode,
		Status:            m.Status,
		CanCreateProjects: m.CanCreateProjects,
	}, s.Overrides[userID], s.Registry.Roles()), true
}

func (m *Mirror) gatherGitea(ctx context.Context, reg registry.Registry) (GiteaState, error) {
	out := GiteaState{
		Users:     map[string]gitea.User{},
		Orgs:      map[string]bool{},
		Teams:     map[string]map[string]TeamState{},
		Repos:     map[string]map[string]RepoState{},
		Hooks:     map[string][]gitea.Hook{},
		BotTokens: map[string][]gitea.AccessToken{},
	}

	users, err := m.Gitea.ListUsers(ctx)
	if err != nil {
		return out, fmt.Errorf("the user list: %w", err)
	}
	for _, u := range users {
		out.Users[u.Login] = u
	}

	for _, g := range reg.Groups {
		// A bot's tokens belong to the bot, not to its org: they are read
		// whether or not the org exists yet, so a pass never mints a
		// generation for a bot that already holds a live one.
		for _, prj := range g.Projects {
			if prj.Kind != roles.KindMate {
				continue
			}
			login := BotLogin(prj.ID)
			if _, exists := out.Users[login]; !exists {
				continue
			}
			tokens, err := m.Gitea.ListTokens(ctx, login)
			if err != nil {
				return out, fmt.Errorf("tokens of %s: %w", login, err)
			}
			out.BotTokens[login] = tokens
		}

		if _, err := m.Gitea.GetOrg(ctx, g.Slug); err != nil {
			if !gitea.IsNotFound(err) {
				return out, fmt.Errorf("org %s: %w", g.Slug, err)
			}
			continue
		}
		out.Orgs[g.Slug] = true

		teams, err := m.Gitea.ListTeams(ctx, g.Slug)
		if err != nil {
			return out, fmt.Errorf("teams of %s: %w", g.Slug, err)
		}
		out.Teams[g.Slug] = map[string]TeamState{}
		for _, t := range teams {
			members, err := m.Gitea.ListTeamMembers(ctx, t.ID)
			if err != nil {
				return out, fmt.Errorf("members of %s/%s: %w", g.Slug, t.Name, err)
			}
			state := TeamState{ID: t.ID, Members: map[string]bool{}}
			for _, u := range members {
				state.Members[u.Login] = true
			}
			out.Teams[g.Slug][t.Name] = state
		}

		out.Repos[g.Slug] = map[string]RepoState{}
		if _, err := m.Gitea.GetRepo(ctx, g.Slug, registry.GroupRepo); err == nil {
			repo := RepoState{
				BranchRules: map[string]gitea.BranchProtection{},
				TagRules:    map[string]gitea.TagProtection{},
			}
			branchRules, err := m.Gitea.ListBranchProtections(ctx, g.Slug, registry.GroupRepo)
			if err != nil {
				return out, fmt.Errorf("branch rules of %s/%s: %w", g.Slug, registry.GroupRepo, err)
			}
			for _, r := range branchRules {
				repo.BranchRules[r.RuleName] = r
			}
			tagRules, err := m.Gitea.ListTagProtections(ctx, g.Slug, registry.GroupRepo)
			if err != nil {
				return out, fmt.Errorf("tag rules of %s/%s: %w", g.Slug, registry.GroupRepo, err)
			}
			for _, r := range tagRules {
				repo.TagRules[r.NamePattern] = r
			}
			out.Repos[g.Slug][registry.GroupRepo] = repo
		} else if !gitea.IsNotFound(err) {
			return out, fmt.Errorf("repo %s/%s: %w", g.Slug, registry.GroupRepo, err)
		}

		hooks, err := m.Gitea.ListOrgHooks(ctx, g.Slug)
		if err != nil {
			return out, fmt.Errorf("hooks of %s: %w", g.Slug, err)
		}
		out.Hooks[g.Slug] = hooks
	}
	return out, nil
}

// Apply performs a plan in order and reports how many landed. A failure is
// recorded and the pass continues: the next pass re-plans from whatever is
// true then, which is the whole point of a reconcile.
func (m *Mirror) Apply(ctx context.Context, plan Plan) (int, []string) {
	applied := 0
	var failures []string
	for _, a := range plan.Actions {
		if err := m.perform(ctx, a); err != nil {
			failures = append(failures, a.String()+": "+err.Error())
			m.log().Warn("mirror action failed", "action", a.String(), "err", err.Error())
			continue
		}
		applied++
	}
	return applied, failures
}

func (m *Mirror) perform(ctx context.Context, a Action) error {
	switch a.Kind {
	case CreateOrg:
		_, err := m.Gitea.CreateOrg(ctx, a.Org, a.FullName)
		return err
	case CreateTeam:
		_, err := m.Gitea.CreateTeam(ctx, a.Org, gitea.NewTeam{
			Name: a.Team, Permission: a.FullName, UnitsMap: gitea.AllRepoUnits(a.FullName),
		})
		return err
	case CreateRepo:
		_, err := m.Gitea.CreateOrgRepo(ctx, a.Org, gitea.NewRepo{
			Name: a.Repo, Description: "The group's recipe, environments and release tags",
		})
		return err
	case SetBranchRule:
		_, err := m.Gitea.CreateBranchProtection(ctx, a.Org, a.Repo, *a.BranchRule)
		if err != nil && gitea.Status(err) == 422 {
			_, err = m.Gitea.EditBranchProtection(ctx, a.Org, a.Repo, a.BranchRule.RuleName, *a.BranchRule)
		}
		return err
	case SetTagRule:
		existing, err := m.Gitea.ListTagProtections(ctx, a.Org, a.Repo)
		if err != nil {
			return err
		}
		for _, r := range existing {
			if r.NamePattern == a.TagRule.NamePattern {
				_, err := m.Gitea.EditTagProtection(ctx, a.Org, a.Repo, r.ID, *a.TagRule)
				return err
			}
		}
		_, err = m.Gitea.CreateTagProtection(ctx, a.Org, a.Repo, *a.TagRule)
		return err
	case CreateHook:
		_, err := m.Gitea.CreateOrgHook(ctx, a.Org, gitea.NewHook{
			URL: a.HookURL, Secret: m.hookSecret, Events: HookEvents,
		})
		return err
	case CreateBot:
		_, err := m.Gitea.CreateUser(ctx, gitea.NewUser{
			Login: a.Login, Email: a.Login + "@bots.invalid", FullName: a.FullName,
			Restricted: true, Visibility: "private",
		})
		return err
	case ShapeBot:
		yes, no, zero := true, false, 0
		_, err := m.Gitea.EditUser(ctx, a.Login, gitea.UserEdit{
			Active: &yes, Restricted: &yes, MaxRepoCreation: &zero,
			AllowCreateOrganization: &no, FullName: &a.FullName,
		})
		return err
	case AddTeamMember:
		id, err := m.teamID(ctx, a.Org, a.Team)
		if err != nil {
			return err
		}
		return m.Gitea.AddTeamMember(ctx, id, a.Login)
	case RemoveTeamMember:
		id, err := m.teamID(ctx, a.Org, a.Team)
		if err != nil {
			return err
		}
		return m.Gitea.RemoveTeamMember(ctx, id, a.Login)
	case PromoteSiteAdmin, DemoteSiteAdmin:
		admin := a.Kind == PromoteSiteAdmin
		_, err := m.Gitea.EditUser(ctx, a.Login, gitea.UserEdit{Admin: &admin})
		return err
	case DeactivatePerson:
		no := false
		_, err := m.Gitea.EditUser(ctx, a.Login, gitea.UserEdit{Active: &no})
		return err
	case DeletePersonTokens:
		tokens, err := m.Gitea.ListTokens(ctx, a.Login)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			if err := m.Gitea.DeleteToken(ctx, a.Login, t.Name); err != nil {
				return err
			}
		}
		return nil
	case DeleteBotToken:
		return m.Gitea.DeleteToken(ctx, a.Login, a.TokenName)
	case DeliverMateAccess:
		return m.deliverMateAccess(ctx, a)
	default:
		return fmt.Errorf("unknown action %q", a.Kind)
	}
}

// SetHookSecret gives the mirror the HMAC secret it puts on the hooks it
// creates. It is never read back out.
func (m *Mirror) SetHookSecret(secret string) { m.hookSecret = secret }

func (m *Mirror) teamID(ctx context.Context, org, team string) (int64, error) {
	teams, err := m.Gitea.ListTeams(ctx, org)
	if err != nil {
		return 0, err
	}
	for _, t := range teams {
		if t.Name == team {
			return t.ID, nil
		}
	}
	return 0, fmt.Errorf("org %s has no team %s", org, team)
}

// Describe renders a plan for a person to read. It never names a token value.
func Describe(p Plan) string {
	lines := make([]string, 0, len(p.Actions)+len(p.Problems))
	for _, a := range p.Actions {
		lines = append(lines, a.String())
	}
	sort.Strings(p.Problems)
	lines = append(lines, p.Problems...)
	if len(p.AwaitingSignIn) > 0 {
		awaiting := append([]string(nil), p.AwaitingSignIn...)
		sort.Strings(awaiting)
		lines = append(lines, fmt.Sprintf("%d with rights and no Gitea account yet, which the first sign-in makes: %s",
			len(awaiting), strings.Join(awaiting, ", ")))
	}
	return strings.Join(lines, "\n")
}
