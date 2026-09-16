package mirror

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/gitea"
)

// AppName is what the Mate app's OAuth2 application is called in Gitea. The
// name is the identity: the broker holds no database, so a pass finds its own
// application by looking for this name among the site admin's.
const AppName = "Zerops Mate"

// CallbackPath is where the app takes Gitea's authorization code back, on
// every origin it runs from.
const CallbackPath = "/gitea/callback"

// AppClient is the public client the Mate app starts its PKCE flow with. It
// carries no secret because there is none: the client is public, and its
// redirect URIs are what protect it.
type AppClient struct {
	ClientID     string
	RedirectURIs []string
}

// CallbackURIs is the callback of every origin, sorted and without repeats —
// the exact list the application must carry.
func CallbackURIs(origins []string) []string {
	var out []string
	for _, origin := range origins {
		uri := strings.TrimSuffix(strings.TrimSpace(origin), "/") + CallbackPath
		if uri != CallbackPath && !slices.Contains(out, uri) {
			out = append(out, uri)
		}
	}
	slices.Sort(out)
	return out
}

// AppClient is the client the last pass registered, if one has.
func (m *Mirror) AppClient() (AppClient, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appClient == nil {
		return AppClient{}, false
	}
	return AppClient{ClientID: m.appClient.ClientID, RedirectURIs: slices.Clone(m.appClient.RedirectURIs)}, true
}

// EnsureAppClient makes the Mate app's OAuth2 application true: one public
// client named [AppName], whose redirect URIs are the callback of every origin
// the app runs from. It is idempotent, and when nothing has changed it is one
// Gitea call — the list.
func (m *Mirror) EnsureAppClient(ctx context.Context) error {
	want := CallbackURIs(m.AppOrigins)
	if len(want) == 0 {
		return nil
	}

	apps, err := m.Gitea.ListOAuth2Apps(ctx)
	if err != nil {
		return fmt.Errorf("the site admin's applications: %w", err)
	}
	var app *gitea.OAuth2App
	for i := range apps {
		if apps[i].Name == AppName {
			app = &apps[i]
			break
		}
	}

	switch {
	case app == nil:
		// confidential_client false is the load-bearing field: it is what lets
		// a browser page with no secret finish the flow, for every person and
		// not only the admin who registered it (ledger 2026-09-16).
		created, err := m.Gitea.CreateOAuth2App(ctx, gitea.NewOAuth2App{
			Name: AppName, ConfidentialClient: false, RedirectURIs: want,
		})
		if err != nil {
			return fmt.Errorf("registering %q: %w", AppName, err)
		}
		app = &created
	case app.ConfidentialClient || !slices.Equal(sorted(app.RedirectURIs), want):
		edited, err := m.Gitea.EditOAuth2App(ctx, app.ID, gitea.NewOAuth2App{
			Name: AppName, ConfidentialClient: false, RedirectURIs: want,
		})
		if err != nil {
			return fmt.Errorf("updating %q: %w", AppName, err)
		}
		app = &edited
	}

	if app.ClientID == "" {
		return fmt.Errorf("%q carries no client id", AppName)
	}
	m.mu.Lock()
	m.appClient = &AppClient{ClientID: app.ClientID, RedirectURIs: want}
	m.mu.Unlock()
	return nil
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
