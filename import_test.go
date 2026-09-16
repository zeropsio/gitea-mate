package giteamate_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Mate app fills these before it sends the document
// (packages/client-runtime/src/zerops/giteaRecipe.ts in the fork). __REGION__
// is the region's *name* — `prg1`, what `zeropsRegionFromPublicZone` returns
// off the freshly created project — and every host the document writes is
// derived from it: `api.app-{region}.zerops.io` for the API and
// `{service}-{subdomainHost}-{port}.{region}.zerops.app` for a subdomain. One
// placeholder, two shapes; a document that spells either of them from the
// other writes a host that does not resolve.
var placeholders = map[string]string{
	"__REGION__":            "prg1",
	"__CORS__":              "http://localhost:5173",
	"__ZEROPS_TOKEN__":      "token-value",
	"__ZEROPS_CLIENT_ID__":  "y6tz5g4lQVaENpmlknyrRw",
	"__ZEROPS_PROJECT_ID__": "m1VrZPJlSnAmnYAvfuEZEg",
	"__MATE_APP_URL__":      "http://localhost:5173",
}

func filledImport(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("import/gitea-project.yaml")
	if err != nil {
		t.Fatalf("reading the import: %v", err)
	}
	// Only the document, not the comment block that explains the placeholders.
	var body []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#zerops") {
			continue
		}
		body = append(body, line)
	}
	out := strings.Join(body, "\n")
	for name, value := range placeholders {
		out = strings.ReplaceAll(out, name, value)
	}
	return out
}

func TestGiteaProjectImportLeavesNoPlaceholder(t *testing.T) {
	filled := filledImport(t)
	if left := regexp.MustCompile(`__[A-Z_]+__`).FindAllString(filled, -1); len(left) > 0 {
		t.Fatalf("placeholders the app does not fill: %v", left)
	}
}

func TestGiteaProjectImportHosts(t *testing.T) {
	filled := filledImport(t)
	for _, want := range []string{
		"https://api.app-prg1.zerops.io",
		"https://web-${zeropsSubdomainHost}-3000.prg1.zerops.app",
		"https://broker-${zeropsSubdomainHost}-8080.prg1.zerops.app",
	} {
		if !strings.Contains(filled, want) {
			t.Errorf("the filled import does not carry %q", want)
		}
	}
	// Nothing may name a host the placeholder built the other way round.
	for _, wrong := range []string{"api.prg1.zerops.io", "app-prg1.zerops.app"} {
		if strings.Contains(filled, wrong) {
			t.Errorf("the filled import carries %q, which does not resolve", wrong)
		}
	}
}

// The Mate app's origins are one list: `web` allows them, and the broker
// registers the app's public OAuth2 client for a callback on each. A document
// that fills __CORS__ for one service only would leave the app a client it
// cannot use from half the shells it runs in.
func TestGiteaProjectImportGivesBothServicesTheSameOrigins(t *testing.T) {
	var doc struct {
		Services []struct {
			Hostname string         `yaml:"hostname"`
			Vault    map[string]any `yaml:"vault"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(filledImport(t)), &doc); err != nil {
		t.Fatalf("the import is not YAML: %v", err)
	}
	want := placeholders["__CORS__"]
	got := map[string]any{}
	for _, s := range doc.Services {
		switch s.Hostname {
		case "web":
			got["web"] = s.Vault["GITEA_CORS_ALLOW_DOMAIN"]
		case "broker":
			got["broker"] = s.Vault["MATE_APP_ORIGINS"]
		}
	}
	for _, hostname := range []string{"web", "broker"} {
		if got[hostname] != want {
			t.Errorf("%s got the origins %v, want %q", hostname, got[hostname], want)
		}
	}
}

func TestGiteaProjectImportServices(t *testing.T) {
	var doc struct {
		Services []struct {
			Hostname    string `yaml:"hostname"`
			Type        string `yaml:"type"`
			ZeropsSetup string `yaml:"zeropsSetup"`
		} `yaml:"services"`
		Project any `yaml:"project"`
	}
	if err := yaml.Unmarshal([]byte(filledImport(t)), &doc); err != nil {
		t.Fatalf("the import is not YAML: %v", err)
	}
	if doc.Project != nil {
		// The app creates the project itself; the platform refuses an import
		// that carries a project block.
		t.Error("the services-only import carries a project block")
	}
	want := map[string]string{"db": "", "volume": "", "web": "gitea", "broker": "broker"}
	got := map[string]string{}
	for _, s := range doc.Services {
		got[s.Hostname] = s.ZeropsSetup
	}
	for hostname, setup := range want {
		if have, ok := got[hostname]; !ok || have != setup {
			t.Errorf("service %q: setup %q, want %q (present=%v)", hostname, have, setup, ok)
		}
	}
}
