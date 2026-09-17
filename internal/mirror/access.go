package mirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// A Mate's Gitea access (docs/broker-api.md, "A Mate's Gitea access"): nobody
// asks for it. The registry says which projects are Mates, and every pass
// makes each one's access true the way it makes teams and bots true — the
// bot, a live token generation, and three variables on the Mate's zcp
// container, written with the broker's Zerops token and never followed by a
// restart, because zcp reads the container's live env store.

// The three variables the loop writes onto a Mate's container
// (docs/vocabulary.md, "A Mate's environment").
const (
	VarGiteaURL   = "GITEA_URL"
	VarBrokerURL  = "MATE_BROKER_URL"
	VarGiteaToken = "GITEA_TOKEN"
)

// zcpTypePrefix is how a Mate's container is told from the rest of its
// project: the service whose type version is zcp@{n}.
const zcpTypePrefix = "zcp@"

// MateService is a Mate's zcp container as a pass read it: the service to
// write to and the variables it holds, by key.
type MateService struct {
	ServiceID string
	Vars      map[string]zerops.ServiceUserData
}

// gatherMates reads, for every registered Mate, its project's zcp container
// and that container's variables. A Mate that cannot be read — the app has
// not granted the broker the project yet, the container is not there yet, the
// platform refused — is a reason in the second map, never an error: the pass
// reports it and tries again next time, and goes on for every other Mate.
func (m *Mirror) gatherMates(ctx context.Context, reg registry.Registry) (map[string]MateService, map[string]string) {
	services := map[string]MateService{}
	problems := map[string]string{}
	for _, g := range reg.Groups {
		for _, prj := range g.Projects {
			if prj.Kind != roles.KindMate {
				continue
			}
			svc, why := m.readMate(ctx, prj.ID)
			if why != "" {
				problems[prj.ID] = why
				continue
			}
			services[prj.ID] = svc
		}
	}
	return services, problems
}

func (m *Mirror) readMate(ctx context.Context, projectID string) (MateService, string) {
	list, err := m.Zerops.Services(ctx, m.ClientID, projectID)
	if err != nil {
		return MateService{}, unreachable("its project's services", err)
	}
	var zcp *zerops.Service
	for i := range list {
		if strings.HasPrefix(list[i].TypeInfo.VersionName, zcpTypePrefix) {
			zcp = &list[i]
			break
		}
	}
	if zcp == nil {
		return MateService{}, "its project has no " + zcpTypePrefix + " service yet, so there is nothing to write to"
	}
	vars, err := m.Zerops.UserData(ctx, zcp.ID)
	if err != nil {
		return MateService{}, unreachable("its container's variables", err)
	}
	out := MateService{ServiceID: zcp.ID, Vars: map[string]zerops.ServiceUserData{}}
	for _, v := range vars {
		out.Vars[v.Key] = v
	}
	return out, ""
}

// unreachable words a failed read. A 403 or 404 is the ordinary state of a
// Mate the app has registered and not yet granted the broker.
func unreachable(what string, err error) string {
	switch status := zerops.Status(err); status {
	case 403, 404:
		return fmt.Sprintf("%s could not be read (%d): the broker's token does not reach the project; the app has not granted it yet", what, status)
	default:
		return fmt.Sprintf("%s could not be read: %v", what, err)
	}
}

// planMateAccess: one delivery per Mate whose container is short of the
// three variables, or whose token is not its bot's newest live generation. A
// GITEA_URL naming another Gitea means the token there is not ours; a token
// that is not the newest generation is one a crash between mint and write
// left behind, and the grace rule will revoke it — so both mint anew. A
// broker URL alone is put right without a mint.
//
// The bots it will mint for are returned, so the same pass revokes nothing of
// theirs.
func (p *planner) planMateAccess() map[string]bool {
	minting := map[string]bool{}
	for _, g := range p.state.Registry.Groups {
		for _, prj := range g.Projects {
			if prj.Kind != roles.KindMate {
				continue
			}
			bot := BotLogin(prj.ID)
			if why, ok := p.state.MateProblems[prj.ID]; ok {
				p.note("Mate %s (%s): %s", prj.ID, p.state.Mates[prj.ID], why)
				continue
			}
			svc, ok := p.state.MateServices[prj.ID]
			if !ok {
				p.note("Mate %s (%s): its container was not read", prj.ID, p.state.Mates[prj.ID])
				continue
			}

			newest, hasLive := newestGeneration(bot, p.state.Gitea.BotTokens[bot])
			token := svc.Vars[VarGiteaToken]
			mint := token.Content == "" ||
				svc.Vars[VarGiteaURL].Content != p.opts.GiteaPublicURL ||
				!hasLive ||
				!strings.HasSuffix(token.Content, newest.TokenLastEight)
			write := mint || svc.Vars[VarBrokerURL].Content != p.opts.BrokerPublicURL
			if !write {
				continue
			}
			if mint {
				minting[bot] = true
			}
			p.do(Action{
				Kind: DeliverMateAccess, Org: g.Slug, Login: bot, FullName: p.state.Mates[prj.ID],
				Project: prj.ID, Service: svc.ServiceID, Mint: mint,
			})
		}
	}
	return minting
}

// newestGeneration is the bot token with the highest generation in its name,
// and whether there is one at all.
func newestGeneration(bot string, tokens []gitea.AccessToken) (gitea.AccessToken, bool) {
	var newest gitea.AccessToken
	best := 0
	for _, t := range tokens {
		if n, ok := ParseTokenName(bot, t.Name); ok && n > best {
			best, newest = n, t
		}
	}
	return newest, best > 0
}

// deliverMateAccess performs one delivery: the bot is made true, a new
// generation is minted when the plan says so, and each of the three variables
// is created when missing, updated when different, and left alone when it
// already holds the value. The container is never restarted.
//
// The variables are read before anything is minted, so a container that
// cannot be read costs no generation; a write that fails after the mint
// leaves a token nobody holds, which the next pass supersedes.
func (m *Mirror) deliverMateAccess(ctx context.Context, a Action) error {
	if err := m.EnsureBot(ctx, a.Org, a.Login, a.FullName); err != nil {
		return fmt.Errorf("the bot: %w", err)
	}
	have, err := m.Zerops.UserData(ctx, a.Service)
	if err != nil {
		return fmt.Errorf("the container's variables: %w", err)
	}
	byKey := map[string]zerops.ServiceUserData{}
	for _, v := range have {
		byKey[v.Key] = v
	}

	want := []zerops.UserDataSpec{
		{Key: VarGiteaURL, Content: m.GiteaPublicURL},
		{Key: VarBrokerURL, Content: m.BrokerPublicURL},
	}
	if a.Mint {
		tokens, err := m.Gitea.ListTokens(ctx, a.Login)
		if err != nil {
			return fmt.Errorf("the bot's tokens: %w", err)
		}
		newest := 0
		for _, t := range tokens {
			if n, ok := ParseTokenName(a.Login, t.Name); ok && n > newest {
				newest = n
			}
		}
		minted, err := m.Gitea.MintToken(ctx, a.Login, TokenName(a.Login, newest+1), BotScopes)
		if err != nil {
			return fmt.Errorf("minting generation %d: %w", newest+1, err)
		}
		want = append(want, zerops.UserDataSpec{Key: VarGiteaToken, Content: minted.Value, Sensitive: true})
	}

	for _, spec := range want {
		cur, exists := byKey[spec.Key]
		switch {
		case !exists:
			if _, err := m.Zerops.CreateUserData(ctx, a.Service, spec); err != nil {
				return fmt.Errorf("creating %s: %w", spec.Key, err)
			}
		case cur.Content != spec.Content:
			if err := m.Zerops.UpdateUserData(ctx, cur.ID, spec.Key, spec.Content); err != nil {
				return fmt.Errorf("updating %s: %w", spec.Key, err)
			}
		}
	}
	return nil
}

// EnsureBot makes a Mate's bot true on its own: created if missing, shaped
// (restricted, creating nothing), and in its group's read team. It is
// idempotent, and it is what a delivery does first, so a Mate is served even
// on a pass where the bot's own actions were planned and one of them failed.
func (m *Mirror) EnsureBot(ctx context.Context, org, bot, mateName string) error {
	if _, err := m.Gitea.GetUser(ctx, bot); err != nil {
		if !gitea.IsNotFound(err) {
			return err
		}
		if _, err := m.Gitea.CreateUser(ctx, gitea.NewUser{
			Login: bot, Email: bot + "@bots.invalid", FullName: mateName,
			Restricted: true, Visibility: "private",
		}); err != nil {
			return err
		}
	}
	yes, no, zero := true, false, 0
	if _, err := m.Gitea.EditUser(ctx, bot, gitea.UserEdit{
		Active: &yes, Restricted: &yes, MaxRepoCreation: &zero, AllowCreateOrganization: &no,
	}); err != nil {
		return err
	}
	id, err := m.teamID(ctx, org, TeamRead)
	if err != nil {
		return err
	}
	return m.Gitea.AddTeamMember(ctx, id, bot)
}
