package mirror_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/gitea/giteatest"
	"github.com/zeropsio/gitea-mate/internal/mirror"
	"github.com/zeropsio/gitea-mate/internal/siteadmin"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

// webService is Gitea's own container on the Gitea project — the service
// that publishes the site admin's pair on its first boot.
const webService = "s-web"

// stale is what the broker's environment can hold: a pair that is not the one
// Gitea knows.
var stale = gitea.AdminCredentials{Token: "stale-token", Password: "stale-password"}

// publish makes web's variables carry the pair the fake Gitea accepts.
func publish(z *zeropstest.Fake) {
	z.SetUserData(webService,
		zerops.ServiceUserData{Key: siteadmin.VarToken, Content: giteatest.AdminToken, Sensitive: true},
		zerops.ServiceUserData{Key: siteadmin.VarPassword, Content: giteatest.AdminPassword, Sensitive: true},
	)
}

// resolvingRig is newRig with the Gitea client asking a siteadmin.Resolver
// for its pair, the way the broker wires it: the environment says stale, and
// web is on the Gitea project. The app's origin is set so the pass also
// registers the OAuth2 client, the first admin call a real pass makes.
func resolvingRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.zerops.SetServices(giteaPrjID,
		zerops.Service{ID: "s-db", ProjectID: giteaPrjID, Name: "db", Status: "ACTIVE"},
		zerops.Service{ID: webService, ProjectID: giteaPrjID, Name: "web", Status: "ACTIVE"},
	)
	resolver := siteadmin.New(siteadmin.Config{
		Env: stale, Zerops: r.zerops.Client("broker"), ClientID: org, ProjectID: giteaPrjID,
		Log: slog.New(slog.DiscardHandler),
	})
	r.mirror.Gitea = gitea.New(gitea.Config{BaseURL: r.gitea.URL(), AdminUser: giteatest.AdminUser, Admin: resolver})
	return r
}

func webReads(z *zeropstest.Fake) int {
	n := 0
	for _, req := range z.Requests {
		if req == "GET /service-stack/"+webService+"/user-data" {
			n++
		}
	}
	return n
}

// The measured failure: the broker holds a pair Gitea refuses. The pass's
// first Gitea call answers 401, the pair is read from web, and the pass goes
// on to build everything.
func TestAPassRefusedByGiteaReadsThePairFromWebAndSucceeds(t *testing.T) {
	r := resolvingRig(t)
	publish(r.zerops)

	result, err := r.mirror.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if result.Applied == 0 || result.Applied != result.Planned || len(result.Failures) != 0 {
		t.Fatalf("applied %d of %d; failures: %v", result.Applied, result.Planned, result.Failures)
	}
	if got := r.gitea.TeamMembers("acme", "read"); !contains(got, "mate-p-fen") {
		t.Errorf("read team = %v", got)
	}
	if n := webReads(r.zerops); n != 1 {
		t.Errorf("web was read %d times, want once: %v", n, r.zerops.Requests)
	}
	// The minted token reached Fen's container: the basic-auth route was
	// refused too and recovered the same way.
	if v := r.vars()[mirror.VarGiteaToken]; v.Content == "" {
		t.Error("Fen's container carries no GITEA_TOKEN")
	}
}

// When web has not published yet either, the pass reports and writes
// nothing; the next pass, once it has, succeeds. Nothing crashes in between.
func TestAPassWhoseReadOfWebFailsReportsAndTriesAgain(t *testing.T) {
	cases := []struct {
		name string
		fail func(z *zeropstest.Fake)
		mend func(z *zeropstest.Fake)
	}{
		{
			name: "web has not published the pair yet",
			fail: func(z *zeropstest.Fake) {},
			mend: publish,
		},
		{
			name: "the platform refuses the read",
			fail: func(z *zeropstest.Fake) {
				publish(z)
				z.Fail["GET /service-stack/"+webService+"/user-data"] = 500
			},
			mend: func(z *zeropstest.Fake) { delete(z.Fail, "GET /service-stack/"+webService+"/user-data") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resolvingRig(t)
			tc.fail(r.zerops)
			ctx := context.Background()

			_, err := r.mirror.Pass(ctx)
			if !errors.Is(err, mirror.ErrUnreadable) {
				t.Fatalf("Pass = %v, want ErrUnreadable", err)
			}
			if strings.Contains(err.Error(), stale.Token) || strings.Contains(err.Error(), giteatest.AdminToken) {
				t.Errorf("the error carries a token: %q", err)
			}
			if r.zerops.Wrote() {
				t.Errorf("a refused pass wrote to the platform: %v", r.zerops.Requests)
			}
			for _, call := range r.gitea.Calls {
				if !strings.HasPrefix(call, "GET ") {
					t.Errorf("a refused pass wrote to Gitea: %s", call)
				}
			}

			tc.mend(r.zerops)
			result, err := r.mirror.Pass(ctx)
			if err != nil {
				t.Fatalf("the next pass: %v", err)
			}
			if result.Applied == 0 || result.Applied != result.Planned {
				t.Fatalf("the next pass applied %d of %d; failures: %v", result.Applied, result.Planned, result.Failures)
			}
		})
	}
}
