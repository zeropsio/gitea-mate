package mirror

import (
	"context"
	"fmt"
	"maps"
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
	return MateService{ServiceID: zcp.ID, Vars: byKey(vars)}, ""
}

// byKey indexes a container's variables by their key in upper case: the
// platform holds one variable per key in any case (400 userDataDuplicateKey,
// "not unique (case insensitive)"), so a `gitea_url` is the GITEA_URL a
// delivery updates, never one it creates beside it.
func byKey(vars []zerops.ServiceUserData) map[string]zerops.ServiceUserData {
	out := make(map[string]zerops.ServiceUserData, len(vars))
	for _, v := range vars {
		out[strings.ToUpper(v.Key)] = v
	}
	return out
}

// unreachable words a failed read. The app mints the broker's token at org
// BASIC_USER, which reaches every Mate as the press registers it; a 403 or 404
// is an older token at org READ_ONLY on a Mate the app has not granted it.
func unreachable(what string, err error) string {
	switch status := zerops.Status(err); status {
	case 403, 404:
		return fmt.Sprintf("%s could not be read (%d): the broker's token does not reach the project; an org READ_ONLY broker token reaches a Mate only once the app has granted it", what, status)
	default:
		return fmt.Sprintf("%s could not be read: %v", what, err)
	}
}

// planMateAccess: one delivery per Mate whose container is short of the
// three variables, or whose token is not its bot's newest live generation. A
// GITEA_URL naming another Gitea means the token there is not ours; a token
// that is not the newest generation is one a crash between mint and write
// left behind — planBotTokens keeps it while the container holds it, so the
// container is minted anew and converges on the newest. A newest generation
// that lacks a scope a bot's token carries now was minted by an earlier
// broker, and a token's scopes never change after it is minted. All three
// mint anew. A broker URL alone is put right without a mint.
func (p *planner) planMateAccess() {
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
				!strings.HasSuffix(token.Content, newest.TokenLastEight) ||
				!coversScopes(newest.Scopes, BotScopes)
			// A variable held under another case is one zcp does not read: it
			// is replaced under its own name, the token as it is, unminted.
			write := mint || svc.Vars[VarBrokerURL].Content != p.opts.BrokerPublicURL ||
				miscased(svc.Vars, VarGiteaURL, VarBrokerURL, VarGiteaToken)
			if !write {
				continue
			}
			p.do(Action{
				Kind: DeliverMateAccess, Org: g.Slug, Login: bot, FullName: p.state.Mates[prj.ID],
				Project: prj.ID, Service: svc.ServiceID, Mint: mint,
			})
		}
	}
}

// miscased reports whether a container holds any of keys under another case.
func miscased(vars map[string]zerops.ServiceUserData, keys ...string) bool {
	for _, k := range keys {
		if v, ok := vars[k]; ok && v.Key != k {
			return true
		}
	}
	return false
}

// coversScopes reports whether a token minted with have may do everything
// want names, read the way Gitea reads scopes: "all" is every scope, and a
// write scope includes the read one of the same category. Gitea lists a
// token's scopes in its own order, so the order is never compared.
func coversScopes(have, want []string) bool {
	held := make(map[string]bool, len(have))
	for _, scope := range have {
		held[scope] = true
	}
	if held["all"] {
		return true
	}
	for _, scope := range want {
		if held[scope] {
			continue
		}
		if category, ok := strings.CutPrefix(scope, "read:"); ok && held["write:"+category] {
			continue
		}
		return false
	}
	return true
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

// deliverMateAccess performs one delivery: the bot is made true, the two
// plain variables are written, and only after them is a new generation minted
// when the plan says so and written as GITEA_TOKEN. Each variable is created
// when missing, updated when different, and left alone when it already holds
// the value. The container is never restarted.
//
// A plain write that fails stops the delivery before a mint. A plain variable
// already holding its value is not written, so that proves nothing: what
// keeps a refusing container from piling generations up is the rollback — a
// generation whose GITEA_TOKEN write is refused (4xx) is deleted again. On an
// ambiguous error the container may hold it, so it stays.
func (m *Mirror) deliverMateAccess(ctx context.Context, a Action) error {
	if err := m.EnsureBot(ctx, a.Org, a.Login, a.FullName); err != nil {
		return fmt.Errorf("the bot: %w", err)
	}
	have, err := m.Zerops.UserData(ctx, a.Service)
	if err != nil {
		return fmt.Errorf("the container's variables: %w", err)
	}
	held := byKey(have)

	for _, spec := range []zerops.UserDataSpec{
		{Key: VarGiteaURL, Content: m.GiteaPublicURL},
		{Key: VarBrokerURL, Content: m.BrokerPublicURL},
	} {
		if err := m.writeVar(ctx, a.Service, held, spec); err != nil {
			return err
		}
	}
	if !a.Mint {
		if tok, ok := held[VarGiteaToken]; ok && tok.Key != VarGiteaToken {
			return m.writeVar(ctx, a.Service, held, zerops.UserDataSpec{Key: VarGiteaToken, Content: tok.Content, Sensitive: true})
		}
		return nil
	}

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
	name := TokenName(a.Login, newest+1)
	minted, err := m.Gitea.MintToken(ctx, a.Login, name, BotScopes)
	if err != nil {
		return fmt.Errorf("minting generation %d: %w", newest+1, err)
	}
	if err := m.writeVar(ctx, a.Service, held, zerops.UserDataSpec{Key: VarGiteaToken, Content: minted.Value, Sensitive: true}); err != nil {
		// A 4xx is a definite refusal: nobody holds the generation just
		// minted, and it goes again. This, not the plain writes, keeps a
		// refusing container from piling generations up; a delete that
		// fails too is returned. Anything else — a 5xx, a timeout — may
		// follow a write the platform committed, so the generation stays and
		// the grace cleanup bounds it.
		if status := zerops.Status(err); status < 400 || status > 499 {
			return err
		}
		if rollback := m.Gitea.DeleteToken(ctx, a.Login, name); rollback != nil {
			return fmt.Errorf("%w; deleting the unheld generation %d: %w", err, newest+1, rollback)
		}
		return err
	}
	return nil
}

// writeVar makes one variable of a container hold spec — an upsert: updated
// by its id when the container holds the key in any case (and renamed to
// spec's own case, the one zcp reads), created only when it holds none, left
// alone when it already holds the value. A create the platform refuses as a
// duplicate means a read listed less than the container holds — the paged
// list of 2026-09-30, or a variable written a moment ago — so the variables
// are read again into held and the one found is updated; a variable no read
// lists returns the refusal, since an update needs its id.
func (m *Mirror) writeVar(ctx context.Context, service string, held map[string]zerops.ServiceUserData, spec zerops.UserDataSpec) error {
	cur, exists := held[strings.ToUpper(spec.Key)]
	if !exists {
		_, err := m.Zerops.CreateUserData(ctx, service, spec)
		if zerops.Code(err) != zerops.CodeUserDataDuplicateKey {
			if err != nil {
				return fmt.Errorf("creating %s: %w", spec.Key, err)
			}
			return nil
		}
		again, readErr := m.Zerops.UserData(ctx, service)
		if readErr != nil {
			return fmt.Errorf("creating %s: %w; reading the container's variables again: %w", spec.Key, err, readErr)
		}
		clear(held)
		maps.Copy(held, byKey(again))
		if cur, exists = held[strings.ToUpper(spec.Key)]; !exists {
			return fmt.Errorf("creating %s: %w, and no read lists it", spec.Key, err)
		}
	}
	if cur.Key != spec.Key {
		return m.replaceVar(ctx, service, held, cur, spec)
	}
	if cur.Content == spec.Content {
		return nil
	}
	if err := m.Zerops.UpdateUserData(ctx, cur.ID, spec.Key, spec.Content); err != nil {
		return fmt.Errorf("updating %s: %w", spec.Key, err)
	}
	return nil
}

// replaceVar moves a variable held under another case to spec's own: it is
// deleted by its id and created anew, two calls whose semantics are known,
// where an update's renaming of a key is not measured. It is one attempt a
// pass — a delivery writes each variable once — and a refusal is said at Info
// and returned, so the next pass tries again and nothing is minted past it.
func (m *Mirror) replaceVar(ctx context.Context, service string, held map[string]zerops.ServiceUserData, cur zerops.ServiceUserData, spec zerops.UserDataSpec) error {
	err := m.Zerops.DeleteUserData(ctx, cur.ID)
	if err == nil {
		delete(held, strings.ToUpper(spec.Key))
		_, err = m.Zerops.CreateUserData(ctx, service, spec)
	}
	if err != nil {
		m.log().Info("a variable held under another case could not be moved to its own name; the next pass tries again",
			"service", service, "key", spec.Key, "err", err.Error())
		return fmt.Errorf("moving %s to its own name: %w", spec.Key, err)
	}
	return nil
}

// EnsureBot makes a Mate's bot true on its own: created if missing, shaped
// (restricted, creating nothing, allowed to sign in — a bot retired while its
// project read as deleted comes back once the project is listed again), and in
// its group's read team. It is
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
		ProhibitLogin: &no,
	}); err != nil {
		return err
	}
	id, err := m.teamID(ctx, org, TeamRead)
	if err != nil {
		return err
	}
	return m.Gitea.AddTeamMember(ctx, id, bot)
}
