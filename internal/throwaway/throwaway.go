// Package throwaway proves a person from a throwaway Zerops integration token.
//
// docs/broker-api.md, "Proving a person": POST /mate/credential and
// POST /oidc/complete take a throwaway the Mate app minted as the person. The
// broker accepts it only when all six conditions hold, in this order, and
// refuses with throwaway_invalid plus a reason otherwise. The caller is the
// token's createdByUser. The app deletes the throwaway right after the call;
// the broker never stores it.
//
// Why it is safe to be handed one: a captured throwaway cannot mint a token,
// raise itself or read a project — only org and own-token metadata (ledger
// 2026-09-15).
package throwaway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zeropsio/gitea-mate/internal/zerops"
)

// The reasons a throwaway is refused, in the order they are checked.
const (
	ReasonTokenDead = "token_dead"
	ReasonWrongOrg  = "wrong_org"
	ReasonHasRights = "has_rights"
	ReasonWrongName = "wrong_name"
	ReasonStale     = "stale"
	ReasonNotMember = "not_member"
)

// ErrorCode is what the API answers when a throwaway is refused.
const ErrorCode = "throwaway_invalid"

// Refusal is a typed refusal. Its Reason is one of the constants above; Err is
// the underlying failure, for the log — never for the answer.
type Refusal struct {
	Reason string
	Err    error
}

func (r *Refusal) Error() string {
	if r.Err != nil {
		return "throwaway refused (" + r.Reason + "): " + r.Err.Error()
	}
	return "throwaway refused (" + r.Reason + ")"
}

func (r *Refusal) Unwrap() error { return r.Err }

func refuse(reason string, err error) *Refusal { return &Refusal{Reason: reason, Err: err} }

// Caller is who the throwaway proves: a person, as the org knows them.
type Caller struct {
	// UserID is the Zerops user id — the token's createdByUser. It is the
	// OIDC `sub` and the seed of the person's Gitea login.
	UserID string
	// Member is the person's row in the org's member list, read with the
	// broker's own token: their org role, status and flags.
	Member zerops.Member
	// TokenID and TokenName identify the throwaway itself, for the log.
	TokenID   string
	TokenName string
}

// DefaultWindow is how far a throwaway's `created` may sit from the API's
// clock.
const DefaultWindow = 5 * time.Minute

// Checker runs the check. Broker is a client on the broker's own token;
// AsCaller builds one on a caller's bearer value.
type Checker struct {
	Broker    *zerops.Client
	AsCaller  func(bearer string) *zerops.Client
	ClientID  string
	GiteaHost string
	Window    time.Duration
}

// NamePrefix is what a Gitea sign-in throwaway's name must start with:
// gitea-signin:{gitea host}: (docs/vocabulary.md).
func (c *Checker) NamePrefix() string { return "gitea-signin:" + c.GiteaHost + ":" }

// Check proves the bearer. A *Refusal means the token is not what it claims;
// any other error is the broker's own trouble reaching Zerops, and the caller
// answers 502, not 401.
func (c *Checker) Check(ctx context.Context, bearer string) (Caller, error) {
	window := c.Window
	if window <= 0 {
		window = DefaultWindow
	}
	if strings.TrimSpace(bearer) == "" {
		return Caller{}, refuse(ReasonTokenDead, fmt.Errorf("no bearer token"))
	}
	caller := c.AsCaller(bearer)

	// 1. It answers at all, and names itself. For an integration token
	//    /user/info's id is the token's own id (ledger 2026-09-15); a session
	//    or personal token answers a user id, which step 2 then fails to find.
	info, err := caller.UserInfo(ctx)
	if err != nil {
		return Caller{}, refuse(ReasonTokenDead, err)
	}
	if info.ID == "" {
		return Caller{}, refuse(ReasonTokenDead, fmt.Errorf("/user/info answered no id"))
	}

	// 2. It can read its own detail in THIS org. A token from another org
	//    cannot read this org's tokens.
	token, err := caller.IntegrationToken(ctx, c.ClientID, info.ID)
	if err != nil {
		return Caller{}, refuse(ReasonWrongOrg, err)
	}

	// 3. It carries no rights of any kind.
	if token.RoleCode != string(roleNoAccess) ||
		len(token.Projects) > 0 ||
		token.CanCreateProjects ||
		token.CanViewFinances ||
		token.CanEditFinances {
		return Caller{}, refuse(ReasonHasRights, fmt.Errorf("role %s, %d project grants", token.RoleCode, len(token.Projects)))
	}

	// 4. It is named for this Gitea, and for nothing else.
	if !strings.HasPrefix(token.Name, c.NamePrefix()) {
		return Caller{}, refuse(ReasonWrongName, fmt.Errorf("name is not %s…", c.NamePrefix()))
	}

	// 5. It was minted moments ago, by the API's clock and never the
	//    container's.
	apiNow := token.APIDate
	if apiNow.IsZero() {
		return Caller{}, fmt.Errorf("the Zerops API sent no Date header; the throwaway's age cannot be judged")
	}
	if token.Created.IsZero() {
		return Caller{}, refuse(ReasonStale, fmt.Errorf("the token has no created time"))
	}
	if age := apiNow.Sub(token.Created); age > window || age < -window {
		return Caller{}, refuse(ReasonStale, fmt.Errorf("minted %s from the API's clock", age.Round(time.Second)))
	}

	// 6. The person who made it is still an active member of the org, read
	//    with the broker's own token — not with the throwaway, which could be
	//    answering for a member who has since left.
	if token.CreatedByUser == "" {
		return Caller{}, refuse(ReasonNotMember, fmt.Errorf("the token names no creator"))
	}
	members, err := c.Broker.Members(ctx, c.ClientID)
	if err != nil {
		return Caller{}, fmt.Errorf("reading the org's members: %w", err)
	}
	for _, m := range members.Members {
		if m.UserID != token.CreatedByUser {
			continue
		}
		if m.Status != statusActive {
			return Caller{}, refuse(ReasonNotMember, fmt.Errorf("the creator is %s", m.Status))
		}
		return Caller{UserID: m.UserID, Member: m, TokenID: token.ID, TokenName: token.Name}, nil
	}
	return Caller{}, refuse(ReasonNotMember, fmt.Errorf("the creator is not in the org's member list"))
}

const statusActive = "ACTIVE"

type role string

const roleNoAccess role = "NO_ACCESS"
