package siteadmin_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/gitea"
	"github.com/zeropsio/gitea-mate/internal/siteadmin"
	"github.com/zeropsio/gitea-mate/internal/zerops"
	"github.com/zeropsio/gitea-mate/internal/zerops/zeropstest"
)

const (
	org        = "org-1"
	giteaPrjID = "p-gitea"
	webService = "s-web"
)

var (
	published = gitea.AdminCredentials{Token: "tok-from-web", Password: "pw-from-web"}
	fromEnv   = gitea.AdminCredentials{Token: "tok-from-env", Password: "pw-from-env"}
	reference = gitea.AdminCredentials{Token: "${web_GITEA_ADMIN_TOKEN}", Password: "${web_GITEA_ADMIN_PASSWORD}"}
)

// newFake is the Gitea project: its web service, which has published the site
// admin's pair, and the broker's own token.
func newFake(t *testing.T) *zeropstest.Fake {
	t.Helper()
	z := zeropstest.New(t, org)
	z.AddIdentity("broker", zeropstest.Identity{UserInfoID: "tok-broker", TokenID: "tok-broker", ClientID: org})
	z.SetProjects(zerops.Project{ID: giteaPrjID, Name: "gitea"})
	z.SetServices(giteaPrjID,
		zerops.Service{ID: "s-db", ProjectID: giteaPrjID, Name: "db", Status: "ACTIVE"},
		zerops.Service{ID: webService, ProjectID: giteaPrjID, Name: "web", Status: "ACTIVE"},
		zerops.Service{ID: "s-broker", ProjectID: giteaPrjID, Name: "broker", Status: "ACTIVE"},
	)
	z.SetUserData(webService,
		zerops.ServiceUserData{Key: "GITEA_DOMAIN", Content: "web.example"},
		zerops.ServiceUserData{Key: siteadmin.VarToken, Content: published.Token, Sensitive: true},
		zerops.ServiceUserData{Key: siteadmin.VarPassword, Content: published.Password, Sensitive: true},
	)
	return z
}

func newResolver(z *zeropstest.Fake, env gitea.AdminCredentials) *siteadmin.Resolver {
	return siteadmin.New(siteadmin.Config{
		Env: env, Zerops: z.Client("broker"), ClientID: org, ProjectID: giteaPrjID,
		Log: slog.New(slog.DiscardHandler),
	})
}

// reads counts the platform calls a resolution costs: the service search and
// the read of web's variables.
func reads(z *zeropstest.Fake) (searches, userData int) {
	for _, r := range z.Requests {
		switch r {
		case "POST /service-stack/search":
			searches++
		case "GET /service-stack/" + webService + "/user-data":
			userData++
		}
	}
	return searches, userData
}

func TestArrived(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"   ", false},
		{"${web_GITEA_ADMIN_TOKEN}", false},
		{"${web_GITEA_ADMIN_PASSWORD}", false},
		{" ${web_GITEA_ADMIN_TOKEN} ", false},
		{"a3f9c1b2e4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a9", true},
		{"K7x$Q9{v}p2", true},
		{"${not a reference", true},
	}
	for _, tc := range cases {
		if got := siteadmin.Arrived(tc.value); got != tc.want {
			t.Errorf("Arrived(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestAPairThatArrivedCostsNoPlatformCall(t *testing.T) {
	z := newFake(t)
	r := newResolver(z, fromEnv)
	got, err := r.Admin(context.Background())
	if err != nil || got != fromEnv {
		t.Fatalf("Admin = %+v, %v; want the environment's pair", got, err)
	}
	if len(z.Requests) != 0 {
		t.Errorf("the platform was asked: %v", z.Requests)
	}
}

func TestAnUnarrivedPairIsReadFromWebOnceAndKept(t *testing.T) {
	cases := []struct {
		name string
		env  gitea.AdminCredentials
	}{
		{"the reference reached the container verbatim", reference},
		{"the container has nothing at all", gitea.AdminCredentials{}},
		{"only the password arrived", gitea.AdminCredentials{Token: reference.Token, Password: "pw-from-env"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := newFake(t)
			r := newResolver(z, tc.env)
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				got, err := r.Admin(ctx)
				if err != nil || got != published {
					t.Fatalf("Admin #%d = %+v, %v; want web's pair", i, got, err)
				}
			}
			if searches, userData := reads(z); searches != 1 || userData != 1 {
				t.Errorf("web was read %d/%d times, want once: %v", searches, userData, z.Requests)
			}
		})
	}
}

func TestARefusalReadsWebAgain(t *testing.T) {
	z := newFake(t)
	r := newResolver(z, fromEnv)
	ctx := context.Background()

	got, err := r.Refused(ctx, fromEnv)
	if err != nil || got != published {
		t.Fatalf("Refused = %+v, %v; want web's pair", got, err)
	}
	if again, err := r.Admin(ctx); err != nil || again != published {
		t.Fatalf("Admin after the refusal = %+v, %v; want web's pair kept", again, err)
	}
	if searches, userData := reads(z); searches != 1 || userData != 1 {
		t.Errorf("web was read %d/%d times, want once: %v", searches, userData, z.Requests)
	}

	// Gitea's first boot minted anew: the next refusal reads again and finds
	// the new pair.
	rotated := gitea.AdminCredentials{Token: "tok-rotated", Password: "pw-rotated"}
	z.SetUserData(webService,
		zerops.ServiceUserData{Key: siteadmin.VarToken, Content: rotated.Token, Sensitive: true},
		zerops.ServiceUserData{Key: siteadmin.VarPassword, Content: rotated.Password, Sensitive: true},
	)
	if got, err := r.Refused(ctx, published); err != nil || got != rotated {
		t.Fatalf("Refused after a rotation = %+v, %v; want the rotated pair", got, err)
	}
}

// Two passes can be refused the same pair at once: the second finds the first
// has already replaced it, and reads nothing.
func TestARefusalOfAPairAlreadyReplacedCostsNothing(t *testing.T) {
	z := newFake(t)
	r := newResolver(z, fromEnv)
	ctx := context.Background()
	if _, err := r.Refused(ctx, fromEnv); err != nil {
		t.Fatal(err)
	}
	got, err := r.Refused(ctx, fromEnv)
	if err != nil || got != published {
		t.Fatalf("second Refused = %+v, %v; want the pair already held", got, err)
	}
	if searches, _ := reads(z); searches != 1 {
		t.Errorf("web was searched %d times, want once: %v", searches, z.Requests)
	}
}

func TestWebThatHasNotPublishedYetIsTriedAgainNextTime(t *testing.T) {
	z := newFake(t)
	z.SetUserData(webService, zerops.ServiceUserData{Key: "GITEA_DOMAIN", Content: "web.example"})
	r := newResolver(z, reference)
	ctx := context.Background()

	if _, err := r.Admin(ctx); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("Admin = %v, want an error naming web", err)
	}
	// It publishes; the next ask finds it.
	z.SetUserData(webService,
		zerops.ServiceUserData{Key: siteadmin.VarToken, Content: published.Token, Sensitive: true},
		zerops.ServiceUserData{Key: siteadmin.VarPassword, Content: published.Password, Sensitive: true},
	)
	if got, err := r.Admin(ctx); err != nil || got != published {
		t.Fatalf("Admin after publication = %+v, %v; want web's pair", got, err)
	}
	if _, userData := reads(z); userData != 2 {
		t.Errorf("web was read %d times, want 2: %v", userData, z.Requests)
	}
}

func TestAProjectWithoutWebIsAnError(t *testing.T) {
	z := newFake(t)
	z.SetServices(giteaPrjID, zerops.Service{ID: "s-db", ProjectID: giteaPrjID, Name: "db", Status: "ACTIVE"})
	_, err := newResolver(z, reference).Admin(context.Background())
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("Admin = %v, want an error naming web", err)
	}
}

func TestAPlatformRefusalIsAnErrorThatNamesNoValue(t *testing.T) {
	cases := []struct {
		name string
		fail string
	}{
		{"the service search", "POST /service-stack/search"},
		{"the read of web's variables", "GET /service-stack/" + webService + "/user-data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := newFake(t)
			z.Fail[tc.fail] = 500
			_, err := newResolver(z, reference).Admin(context.Background())
			if err == nil {
				t.Fatal("Admin succeeded against a platform that refused")
			}
			for _, secret := range []string{published.Token, published.Password} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error carries a value: %q", err)
				}
			}
		})
	}
}
