package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/roles"
	"github.com/zeropsio/gitea-mate/internal/throwaway"
)

// ---------------------------------------------------------------------------
// POST /mate/credential
// ---------------------------------------------------------------------------

type credentialRequest struct {
	Project string `json:"project"`
	Mode    string `json:"mode"`
}

type credentialResponse struct {
	URL        string `json:"url"`
	Org        string `json:"org"`
	Bot        string `json:"bot"`
	Generation int    `json:"generation"`
	Minted     bool   `json:"minted"`
	Token      string `json:"token,omitempty"`
}

// handleCredential is docs/broker-api.md § POST /mate/credential: the Mate app,
// as the person, asks for a Mate's Gitea access.
func (s *Server) handleCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body credentialRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "the body is {\"project\": \"…\", \"mode\": \"ensure\"}")
		return
	}
	if body.Mode == "" {
		body.Mode = "ensure"
	}
	if body.Mode != "ensure" && body.Mode != "rotate" {
		WriteError(w, http.StatusBadRequest, "invalid_request", "mode is ensure or rotate")
		return
	}
	if body.Project == "" {
		WriteError(w, http.StatusBadRequest, "invalid_request", "no project")
		return
	}

	caller, ok := s.prove(w, r)
	if !ok {
		return
	}

	org, err := mirror.ReadOrg(ctx, s.deps.Zerops, s.cfg.ZeropsClientID, s.cfg.ZeropsProjectID)
	if err != nil {
		s.log.Error("the org could not be read", "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}

	group, kind, known := org.Registry.GroupOfProject(body.Project)
	if !known || kind != roles.KindMate {
		WriteError(w, http.StatusNotFound, "not_registered", "that project is not a Mate of any registered group")
		return
	}

	member, found := org.Person(caller.UserID)
	if !found {
		WriteError(w, http.StatusForbidden, "not_owner", "you are not a member of this organisation")
		return
	}
	person := roles.Person{ID: member.UserID, OrgRole: member.RoleCode, Status: member.Status, CanCreateProjects: member.CanCreateProjects}
	// The contract: the caller is the project's effective OWNER, or an org
	// OWNER/ADMIN.
	effective := roles.Effective(person, org.Overrides[caller.UserID], body.Project)
	if !(effective == roles.Owner || (member.Status == roles.StatusActive && member.RoleCode.AtLeast(roles.Admin))) {
		WriteError(w, http.StatusForbidden, "not_owner", "only this Mate's owner, or an owner or admin of the organisation, may ask for its Gitea access")
		return
	}

	bot := mirror.BotLogin(body.Project)
	if err := s.ensureBot(ctx, group.Slug, bot, org.Mates[body.Project]); err != nil {
		s.log.Error("the bot could not be made", "bot", bot, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "the Mate's Gitea account could not be made")
		return
	}

	tokens, err := s.deps.Gitea.ListTokens(ctx, bot)
	if err != nil {
		s.log.Error("the bot's tokens could not be read", "bot", bot, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Gitea could not be reached")
		return
	}
	newest := 0
	for _, t := range tokens {
		if n, ok := mirror.ParseTokenName(bot, t.Name); ok && n > newest {
			newest = n
		}
	}

	answer := credentialResponse{URL: s.cfg.GiteaPublicURL, Org: group.Slug, Bot: bot, Generation: newest}
	// ensure with a live token mints nothing; ensure with none, and rotate,
	// mint generation n+1. Older generations are revoked by the rights loop
	// once the newest is ten minutes old — never here.
	if body.Mode == "ensure" && newest > 0 {
		WriteJSON(w, http.StatusOK, answer)
		return
	}

	minted, err := s.deps.Gitea.MintToken(ctx, bot, mirror.TokenName(bot, newest+1), mirror.BotScopes)
	if err != nil {
		s.log.Error("the bot's token could not be minted", "bot", bot, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "the Mate's Gitea token could not be minted")
		return
	}
	answer.Generation = newest + 1
	answer.Minted = true
	answer.Token = minted.Value
	WriteJSON(w, http.StatusOK, answer)
}

// ensureBot makes the Mate's bot if it is missing and puts it in its group's
// read team. It is idempotent: a bot that already exists is only reshaped.
func (s *Server) ensureBot(ctx context.Context, org, bot, mateName string) error {
	if _, err := s.deps.Gitea.GetUser(ctx, bot); err != nil {
		if !gitea.IsNotFound(err) {
			return err
		}
		if _, err := s.deps.Gitea.CreateUser(ctx, gitea.NewUser{
			Login: bot, Email: bot + "@bots.invalid", FullName: mateName,
			Restricted: true, Visibility: "private",
		}); err != nil {
			return err
		}
	}
	yes, no, zero := true, false, 0
	if _, err := s.deps.Gitea.EditUser(ctx, bot, gitea.UserEdit{
		Active: &yes, Restricted: &yes, MaxRepoCreation: &zero, AllowCreateOrganization: &no,
	}); err != nil {
		return err
	}

	teams, err := s.deps.Gitea.ListTeams(ctx, org)
	if err != nil {
		return err
	}
	for _, t := range teams {
		if t.Name != mirror.TeamRead {
			continue
		}
		return s.deps.Gitea.AddTeamMember(ctx, t.ID, bot)
	}
	return errors.New("the group's org has no read team yet; the rights loop makes one")
}

// ---------------------------------------------------------------------------
// POST /mate/repository
// ---------------------------------------------------------------------------

type repositoryRequest struct {
	Name string `json:"name"`
}

type repositoryResponse struct {
	FullName      string `json:"fullName"`
	CloneURL      string `json:"cloneUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Created       bool   `json:"created"`
}

// handleRepository is docs/broker-api.md § POST /mate/repository: a Mate, with
// its bot's Gitea token, asks for a service repository.
func (s *Server) handleRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body repositoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Name == "" {
		WriteError(w, http.StatusBadRequest, "invalid_request", "the body is {\"name\": \"api\"}")
		return
	}
	if !repoNamePattern.MatchString(body.Name) {
		WriteError(w, http.StatusBadRequest, "invalid_request", "a service repository is named ^[a-z][a-z0-9-]{0,39}$")
		return
	}

	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "token "))
	if token == "" {
		WriteError(w, http.StatusUnauthorized, "not_a_bot", "this endpoint takes a Mate's Gitea token")
		return
	}
	// The broker resolves the token against Gitea; the login names the bot, and
	// the bot names its project.
	who, err := s.deps.Gitea.AsToken(token).WhoAmI(ctx)
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "not_a_bot", "Gitea does not know that token")
		return
	}
	projectID, ok := strings.CutPrefix(who.Login, "mate-")
	if !ok || projectID == "" {
		WriteError(w, http.StatusForbidden, "not_a_bot", "that token does not belong to a Mate's bot")
		return
	}

	org, err := mirror.ReadOrg(ctx, s.deps.Zerops, s.cfg.ZeropsClientID, s.cfg.ZeropsProjectID)
	if err != nil {
		s.log.Error("the org could not be read", "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return
	}
	group, kind, known := org.Registry.GroupOfProject(projectID)
	if !known || kind != roles.KindMate {
		WriteError(w, http.StatusNotFound, "not_registered", "that Mate's project is not in any registered group")
		return
	}

	existing, err := s.deps.Gitea.GetRepo(ctx, group.Slug, body.Name)
	switch {
	case err == nil:
		// Idempotent: a repository this bot already collaborates on answers 200;
		// one it does not is taken.
		collaborator, err := s.deps.Gitea.IsCollaborator(ctx, group.Slug, body.Name, who.Login)
		if err != nil {
			s.log.Error("the collaborators could not be read", "repo", existing.FullName, "err", err.Error())
			WriteError(w, http.StatusBadGateway, "upstream", "Gitea could not be reached")
			return
		}
		if !collaborator {
			WriteError(w, http.StatusConflict, "taken", "that name already belongs to another repository of this group")
			return
		}
		WriteJSON(w, http.StatusOK, repositoryResponse{
			FullName: existing.FullName, CloneURL: s.cloneURL(existing.FullName),
			DefaultBranch: existing.DefaultBranch, Created: false,
		})
		return
	case !gitea.IsNotFound(err):
		s.log.Error("the repository could not be read", "org", group.Slug, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Gitea could not be reached")
		return
	}

	created, err := s.deps.Gitea.CreateOrgRepo(ctx, group.Slug, gitea.NewRepo{
		Name: body.Name, Description: "A service of " + group.Slug,
	})
	if err != nil {
		if gitea.IsConflict(err) {
			WriteError(w, http.StatusConflict, "taken", "that name already belongs to another repository of this group")
			return
		}
		s.log.Error("the repository could not be made", "org", group.Slug, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "the repository could not be made")
		return
	}

	// main: no direct push for anyone, merge by the write team. The rule is
	// written before anything is pushed, so a Mate lands through pull requests
	// from its first commit.
	if _, err := s.deps.Gitea.CreateBranchProtection(ctx, group.Slug, body.Name, gitea.BranchProtection{
		RuleName: "main", EnablePush: false,
		EnableMergeWhitelist: true, MergeWhitelistTeams: []string{mirror.TeamWrite},
		BlockAdminMergeOverride: true,
	}); err != nil {
		s.log.Error("main could not be protected", "repo", created.FullName, "err", err.Error())
	}
	if err := s.deps.Gitea.AddCollaborator(ctx, group.Slug, body.Name, who.Login, "write"); err != nil {
		s.log.Error("the bot could not be made a collaborator", "repo", created.FullName, "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "the repository was made but the Mate could not be given write")
		return
	}

	WriteJSON(w, http.StatusOK, repositoryResponse{
		FullName: created.FullName, CloneURL: s.cloneURL(created.FullName),
		DefaultBranch: created.DefaultBranch, Created: true,
	})
}

// cloneURL is the canonical clone URL — never with a .git suffix, because the
// platform's clone preflight fails on one.
func (s *Server) cloneURL(fullName string) string {
	return s.cfg.GiteaPublicURL + "/" + fullName
}

// ---------------------------------------------------------------------------

// prove runs the throwaway check and writes the refusal itself. It answers
// false when the caller is not proved.
func (s *Server) prove(w http.ResponseWriter, r *http.Request) (throwaway.Caller, bool) {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	caller, err := s.deps.Throwaway.Check(r.Context(), bearer)
	var refusal *throwaway.Refusal
	switch {
	case errors.As(err, &refusal):
		WriteErrorReason(w, http.StatusUnauthorized, throwaway.ErrorCode,
			"that token does not prove a person", refusal.Reason)
		return throwaway.Caller{}, false
	case err != nil:
		s.log.Error("the throwaway could not be checked", "err", err.Error())
		WriteError(w, http.StatusBadGateway, "upstream", "Zerops could not be reached")
		return throwaway.Caller{}, false
	}
	return caller, true
}
