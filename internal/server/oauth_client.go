package server

import (
	"net/http"
	"slices"
)

// OAuthClient is the Mate app's public OAuth2 client in Gitea, as the rights
// loop registered it. There is no secret in it because the client has none:
// it is a browser page, it uses PKCE, and its redirect URIs are what protect
// it.
type OAuthClient struct {
	ClientID     string
	RedirectURIs []string
}

// oauthClientBody is what GET /gitea/oauth-client answers. The two URLs are
// Gitea's own endpoints, so the app has everything the flow needs from one
// call and never spells a Gitea path itself.
type oauthClientBody struct {
	ClientID     string   `json:"clientId"`
	RedirectURIs []string `json:"redirectUris"`
	AuthorizeURL string   `json:"authorizeUrl"`
	TokenURL     string   `json:"tokenUrl"`
}

func (s *Server) handleOAuthClient(w http.ResponseWriter, r *http.Request) {
	s.appCORS(w, r)

	client, ok := s.deps.OAuthClient()
	if !ok {
		WriteError(w, http.StatusServiceUnavailable, "not_registered_yet",
			"the rights loop has not registered the app's OAuth2 client yet; ask again in a moment")
		return
	}
	WriteJSON(w, http.StatusOK, oauthClientBody{
		ClientID:     client.ClientID,
		RedirectURIs: client.RedirectURIs,
		AuthorizeURL: s.cfg.GiteaPublicURL + "/login/oauth/authorize",
		TokenURL:     s.cfg.GiteaPublicURL + "/login/oauth/access_token",
	})
}

func (s *Server) handleOAuthClientPreflight(w http.ResponseWriter, r *http.Request) {
	s.appCORS(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// appCORS allows the origins the Mate app runs from and nothing else. The
// origin is matched literally: a runner in this project can reach this port,
// and localhost is not 127.0.0.1.
func (s *Server) appCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" || !slices.Contains(s.cfg.MateAppOrigins, origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
}
