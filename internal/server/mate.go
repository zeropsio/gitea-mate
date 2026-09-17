package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
)

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
	// The group repository is no Mate's to write, made yet or not: a Mate's
	// recipe reaches it as a pull request from its bot's fork (D23).
	if body.Name == registry.GroupRepo {
		WriteError(w, http.StatusConflict, "taken", "the group repository takes a Mate's changes only as a pull request from its fork")
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
		// Idempotent, and how a group's second Mate joins its app: a service
		// repository that exists is the group's codebase — the recipe's
		// buildFromGit names it for every Mate the recipe creates — so a Mate of
		// the group that does not collaborate on it yet is given the write the
		// first Mate holds. main stays behind pull requests either way.
		collaborator, err := s.deps.Gitea.IsCollaborator(ctx, group.Slug, body.Name, who.Login)
		if err != nil {
			s.log.Error("the collaborators could not be read", "repo", existing.FullName, "err", err.Error())
			WriteError(w, http.StatusBadGateway, "upstream", "Gitea could not be reached")
			return
		}
		if !collaborator {
			if err := s.deps.Gitea.AddCollaborator(ctx, group.Slug, body.Name, who.Login, "write"); err != nil {
				s.log.Error("the bot could not join the repository", "repo", existing.FullName, "err", err.Error())
				WriteError(w, http.StatusBadGateway, "upstream", "the Mate could not be given write on that repository")
				return
			}
			s.log.Info("a Mate joined a repository of its group", "repo", existing.FullName, "bot", who.Login)
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
