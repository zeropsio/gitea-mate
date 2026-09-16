package roles

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

// fixtureFile mirrors fixtures.json, which is the contract: the Mate fork
// replays the same cases against its TypeScript twin.
type fixtureFile struct {
	Version  int       `json:"version"`
	About    string    `json:"about"`
	Registry Registry  `json:"registry"`
	Cases    []fixture `json:"cases"`
}

type fixture struct {
	Name      string          `json:"name"`
	Registry  *Registry       `json:"registry"`
	Person    Person          `json:"person"`
	Overrides map[string]Role `json:"overrides"`
	Expect    Rights          `json:"expect"`
}

func loadFixtures(t *testing.T) fixtureFile {
	t.Helper()
	raw, err := os.ReadFile("fixtures.json")
	if err != nil {
		t.Fatalf("fixtures.json: %v", err)
	}
	var f fixtureFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("fixtures.json: %v", err)
	}
	if f.Version != 1 {
		t.Fatalf("fixtures.json version = %d, want 1", f.Version)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixtures.json has no cases")
	}
	return f
}

func TestComputeAgainstFixtures(t *testing.T) {
	f := loadFixtures(t)
	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			registry := f.Registry
			if tc.Registry != nil {
				registry = *tc.Registry
			}
			got := Compute(tc.Person, tc.Overrides, registry)

			want := tc.Expect
			if want.Claims == nil {
				want.Claims = []string{}
			}
			if want.Mates == nil {
				want.Mates = map[string]string{}
			}
			if want.Groups == nil {
				want.Groups = map[string]GroupRights{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Compute() mismatch\n got: %s\nwant: %s", mustJSON(t, got), mustJSON(t, want))
			}
			if !sort.StringsAreSorted(got.Claims) {
				t.Errorf("claims are not sorted: %v", got.Claims)
			}
		})
	}
}

// The fixture file drives both implementations, so a case that stops covering
// a rule is a silent hole. These assertions keep the set honest.
func TestFixturesCoverEveryOutcome(t *testing.T) {
	f := loadFixtures(t)
	seen := map[string]bool{}
	for _, tc := range f.Cases {
		registry := f.Registry
		if tc.Registry != nil {
			registry = *tc.Registry
		}
		got := Compute(tc.Person, tc.Overrides, registry)
		for _, state := range got.Mates {
			seen["mate:"+state] = true
		}
		for _, g := range got.Groups {
			for _, flag := range []struct {
				n string
				v bool
			}{{"read", g.Read}, {"write", g.Write}, {"release", g.Release}} {
				if flag.v {
					seen["group:"+flag.n] = true
				}
			}
		}
		if got.SiteAdmin {
			seen["siteAdmin"] = true
		}
		if !got.Active {
			seen["inactive"] = true
		}
		if got.CanCreate {
			seen["canCreate"] = true
		}
	}
	for _, want := range []string{
		"mate:open", "mate:listed", "mate:hidden",
		"group:read", "group:write", "group:release",
		"siteAdmin", "inactive", "canCreate",
	} {
		if !seen[want] {
			t.Errorf("no fixture case produces %s", want)
		}
	}
}

func TestReleaseWithoutProduction(t *testing.T) {
	// docs/roles.md: until a group has a production project, its releasers are
	// the org's owners and admins — the recipe is merged before production
	// exists. Every rank, in one table.
	registry := Registry{Groups: []Group{{
		ID: "g", Slug: "g", Projects: []Project{{ID: "p", Kind: KindMate}},
	}}}
	cases := []struct {
		role Role
		want bool
	}{
		{NoAccess, false}, {ReadOnly, false}, {BasicUser, false}, {Admin, true}, {Owner, true},
	}
	for _, tc := range cases {
		got := Compute(Person{ID: "u", OrgRole: tc.role, Status: StatusActive}, nil, registry)
		if got.Groups["g"].Release != tc.want {
			t.Errorf("%s: release = %v, want %v", tc.role, got.Groups["g"].Release, tc.want)
		}
	}
}

func TestUnknownRoleGrantsNothing(t *testing.T) {
	registry := Registry{Groups: []Group{{
		ID: "g", Slug: "g", Projects: []Project{{ID: "p", Kind: KindMate}, {ID: "q", Kind: KindProduction}},
	}}}
	got := Compute(Person{ID: "u", OrgRole: Role("SUPER_OWNER"), Status: StatusActive}, nil, registry)
	if got.SiteAdmin || got.CanCreate || got.Groups["g"] != (GroupRights{}) || got.Mates["p"] != MateHidden {
		t.Errorf("an unknown role granted something: %s", mustJSON(t, got))
	}
}

func TestTeamsFor(t *testing.T) {
	cases := []struct {
		in   GroupRights
		want []string
	}{
		{GroupRights{}, nil},
		{GroupRights{Read: true}, []string{"read"}},
		{GroupRights{Read: true, Write: true}, []string{"read", "write"}},
		{GroupRights{Read: true, Write: true, Release: true}, []string{"read", "write", "release"}},
	}
	for _, tc := range cases {
		if got := tc.in.TeamsFor(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TeamsFor(%+v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLogin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"y6tz5g4lQVaENpmlknyrRw", "u-y6tz5g4lqvaenpmlknyrrw"},
		{"m1VrZPJlSnAmnYAvfuEZEg", "u-m1vrzpjlsnamnyavfuezeg"},
		{"a-b_c/d+e=", "u-abcde"},
		{"", "u-"},
	}
	for _, tc := range cases {
		if got := Login(tc.in); got != tc.want {
			t.Errorf("Login(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestEffective(t *testing.T) {
	person := Person{ID: "u", OrgRole: ReadOnly, Status: StatusActive}
	overrides := map[string]Role{"p-own": Owner}

	cases := []struct {
		name      string
		person    Person
		projectID string
		want      Role
	}{
		{"an override wins", person, "p-own", Owner},
		{"the org role otherwise", person, "p-other", ReadOnly},
		{"an inactive person holds nothing, override or not", Person{ID: "u", OrgRole: Owner, Status: "INVITED"}, "p-own", NoAccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Effective(tc.person, overrides, tc.projectID); got != tc.want {
				t.Errorf("Effective = %q, want %q", got, tc.want)
			}
		})
	}
}
