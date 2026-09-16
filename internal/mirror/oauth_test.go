package mirror_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/mirror"
)

var appOrigins = []string{"https://app.example", "http://localhost:5173"}

func wantCallbacks() []string {
	return []string{"http://localhost:5173/gitea/callback", "https://app.example/gitea/callback"}
}

func TestCallbackURIs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nothing at all", in: nil, want: nil},
		{name: "sorted, whatever order they arrive in", in: appOrigins, want: wantCallbacks()},
		{
			name: "a trailing slash is not a second origin",
			in:   []string{"https://app.example/", "https://app.example"},
			want: []string{"https://app.example/gitea/callback"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mirror.CallbackURIs(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("CallbackURIs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The app is a browser page: it cannot register its own client, so a pass
// does it, and every origin the app runs from gets a callback.
func TestPassRegistersTheAppsPublicClient(t *testing.T) {
	r := newRig(t)
	r.mirror.AppOrigins = appOrigins
	ctx := context.Background()

	if _, ok := r.mirror.AppClient(); ok {
		t.Fatal("a client before the first pass")
	}
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	apps := r.gitea.OAuthApps()
	if len(apps) != 1 {
		t.Fatalf("applications = %+v, want exactly one", apps)
	}
	app := apps[0]
	if app.Name != "Zerops Mate" {
		t.Errorf("the application is called %q", app.Name)
	}
	if app.ConfidentialClient {
		t.Error("the application is confidential; the app holds no secret")
	}
	if !slices.Equal(app.RedirectURIs, wantCallbacks()) {
		t.Errorf("redirect URIs = %v, want %v", app.RedirectURIs, wantCallbacks())
	}

	client, ok := r.mirror.AppClient()
	if !ok || client.ClientID != app.ClientID || !slices.Equal(client.RedirectURIs, wantCallbacks()) {
		t.Errorf("AppClient() = %+v %v, want the registered client", client, ok)
	}
}

// Nothing changed means one call: the list.
func TestSecondPassOnlyListsTheApplications(t *testing.T) {
	r := newRig(t)
	r.mirror.AppOrigins = appOrigins
	ctx := context.Background()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, _ := r.mirror.AppClient()
	r.gitea.ResetCalls()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if r.gitea.Wrote() {
		t.Errorf("the second pass wrote to Gitea: %v", r.gitea.Calls)
	}
	var lists int
	for _, call := range r.gitea.Calls {
		if strings.HasPrefix(call, "GET /user/applications/oauth2") {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("the second pass listed the applications %d times, want once", lists)
	}
	if again, _ := r.mirror.AppClient(); again.ClientID != first.ClientID {
		t.Errorf("the client id changed across passes: %q then %q", first.ClientID, again.ClientID)
	}
}

// A shell moves, an origin is added, the same client serves it: the list is
// replaced and the client id — which the app may already hold — is kept.
func TestAnAddedOriginReplacesTheRedirectList(t *testing.T) {
	r := newRig(t)
	r.mirror.AppOrigins = []string{"https://app.example"}
	ctx := context.Background()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	before, _ := r.mirror.AppClient()

	r.mirror.AppOrigins = appOrigins
	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	apps := r.gitea.OAuthApps()
	if len(apps) != 1 {
		t.Fatalf("applications = %+v, want exactly one", apps)
	}
	if !slices.Equal(apps[0].RedirectURIs, wantCallbacks()) {
		t.Errorf("redirect URIs = %v, want %v", apps[0].RedirectURIs, wantCallbacks())
	}
	after, ok := r.mirror.AppClient()
	if !ok || after.ClientID != before.ClientID {
		t.Errorf("client id %q became %q", before.ClientID, after.ClientID)
	}
}

// An application somebody made confidential, or pointed elsewhere, is put
// right by the next pass.
func TestAConfidentialApplicationIsPutRight(t *testing.T) {
	r := newRig(t)
	r.mirror.AppOrigins = appOrigins
	r.gitea.AddOAuthApp(gitea.OAuth2App{
		Name: "Zerops Mate", ConfidentialClient: true,
		RedirectURIs: wantCallbacks(),
	})
	ctx := context.Background()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	apps := r.gitea.OAuthApps()
	if len(apps) != 1 || apps[0].ConfidentialClient {
		t.Fatalf("applications = %+v, want the one, public", apps)
	}
}

// Gitea refusing the applications is a failure the pass reports; the rights
// it was there to mirror are still written.
func TestAFailedRegistrationDoesNotStopThePass(t *testing.T) {
	r := newRig(t)
	r.mirror.AppOrigins = appOrigins
	r.gitea.Fail["GET /user/applications/oauth2"] = 500
	ctx := context.Background()

	result, err := r.mirror.Pass(ctx)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if result.Applied == 0 || result.Applied != result.Planned {
		t.Errorf("the pass applied %d of %d", result.Applied, result.Planned)
	}
	var named bool
	for _, f := range result.Failures {
		if strings.Contains(f, "OAuth2 client") {
			named = true
		}
	}
	if !named {
		t.Errorf("failures = %v, none names the OAuth2 client", result.Failures)
	}
	if _, ok := r.mirror.AppClient(); ok {
		t.Error("a client is served although none was registered")
	}
}

// No origins configured, no application: the broker registers nothing it was
// not told to.
func TestNoOriginsRegistersNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	if _, err := r.mirror.Pass(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if apps := r.gitea.OAuthApps(); len(apps) != 0 {
		t.Errorf("applications = %+v", apps)
	}
	for _, call := range r.gitea.Calls {
		if strings.Contains(call, "applications/oauth2") {
			t.Errorf("the pass called %q", call)
		}
	}
}
