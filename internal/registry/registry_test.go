package registry_test

import (
	"reflect"
	"testing"

	"github.com/zeropsio/gitea-mate/internal/registry"
	"github.com/zeropsio/gitea-mate/internal/roles"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name         string
		tags         []string
		want         registry.Registry
		wantProblems []string // Problem.Tag values, sorted
	}{
		{
			name: "a whole account: two groups, their projects, a release switch and a leaver",
			tags: []string{
				"mate:tool:gitea",
				"mate:gn:g-acme:acme",
				"mate:gm:g-acme:p-fen:mate",
				"mate:gm:g-acme:p-prod:production",
				"mate:gm:g-acme:p-stage:stage",
				"mate:release:g-acme:mates",
				"mate:gn:g-beta:beta",
				"mate:gm:g-beta:p-bmate:mate",
				"mate:leaving:u-gone",
			},
			want: registry.Registry{
				Groups: []registry.Group{
					{ID: "g-acme", Slug: "acme", MatesMayRelease: true, Projects: []roles.Project{
						{ID: "p-fen", Kind: roles.KindMate},
						{ID: "p-prod", Kind: roles.KindProduction},
						{ID: "p-stage", Kind: roles.KindStage},
					}},
					{ID: "g-beta", Slug: "beta", Projects: []roles.Project{
						{ID: "p-bmate", Kind: roles.KindMate},
					}},
				},
				Leaving: []string{"u-gone"},
			},
		},
		{
			name: "no tags at all",
			tags: nil,
			want: registry.Registry{Groups: []registry.Group{}},
		},
		{
			name: "every tag that is not ours is ignored without a word",
			tags: []string{"mate", "mate:tool:gitea", "mate:g:g-acme", "mate:role:prod", "mate:bot:Fen", "production"},
			want: registry.Registry{Groups: []registry.Group{}},
		},
		{
			name:         "a group tag with no slug",
			tags:         []string{"mate:gn:g-acme"},
			want:         registry.Registry{Groups: []registry.Group{}},
			wantProblems: []string{"mate:gn:g-acme"},
		},
		{
			name:         "a slug that is not a Gitea org name",
			tags:         []string{"mate:gn:g-acme:Acme Corp"},
			want:         registry.Registry{Groups: []registry.Group{}},
			wantProblems: []string{"mate:gn:g-acme:Acme Corp"},
		},
		{
			name: "two groups claiming one slug: the first stands, the second is reported",
			tags: []string{"mate:gn:g-a:acme", "mate:gn:g-b:acme"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme"},
			}},
			wantProblems: []string{"mate:gn:g-b:acme"},
		},
		{
			name: "one group claiming two slugs: the first stands",
			tags: []string{"mate:gn:g-a:acme", "mate:gn:g-a:other"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme"},
			}},
			wantProblems: []string{"mate:gn:g-a:other"},
		},
		{
			name: "a second production in one group is refused, the first stands",
			tags: []string{
				"mate:gn:g-a:acme",
				"mate:gm:g-a:p-1:production",
				"mate:gm:g-a:p-2:production",
			},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme", Projects: []roles.Project{{ID: "p-1", Kind: roles.KindProduction}}},
			}},
			wantProblems: []string{"mate:gm:g-a:p-2:production"},
		},
		{
			name:         "a membership naming a group nobody registered",
			tags:         []string{"mate:gm:g-ghost:p-1:mate"},
			want:         registry.Registry{Groups: []registry.Group{}},
			wantProblems: []string{"mate:gm:g-ghost:p-1:mate"},
		},
		{
			name: "a membership with a kind that is not one of the three",
			tags: []string{"mate:gn:g-a:acme", "mate:gm:g-a:p-1:preview"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme"},
			}},
			wantProblems: []string{"mate:gm:g-a:p-1:preview"},
		},
		{
			name: "a membership missing a field",
			tags: []string{"mate:gn:g-a:acme", "mate:gm:g-a:p-1"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme"},
			}},
			wantProblems: []string{"mate:gm:g-a:p-1"},
		},
		{
			name: "one project claiming two kinds: the first stands",
			tags: []string{"mate:gn:g-a:acme", "mate:gm:g-a:p-1:mate", "mate:gm:g-a:p-1:stage"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme", Projects: []roles.Project{{ID: "p-1", Kind: roles.KindMate}}},
			}},
			wantProblems: []string{"mate:gm:g-a:p-1:stage"},
		},
		{
			name: "a duplicate membership tag is simply the same fact twice",
			tags: []string{"mate:gn:g-a:acme", "mate:gm:g-a:p-1:mate", "mate:gm:g-a:p-1:mate"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme", Projects: []roles.Project{{ID: "p-1", Kind: roles.KindMate}}},
			}},
		},
		{
			name: "a release switch for an unknown group, and a malformed one",
			tags: []string{"mate:gn:g-a:acme", "mate:release:g-ghost:mates", "mate:release:g-a:everyone"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme"},
			}},
			wantProblems: []string{"mate:release:g-a:everyone", "mate:release:g-ghost:mates"},
		},
		{
			name:         "a leaving mark with no user, and one with a stray colon",
			tags:         []string{"mate:leaving:", "mate:leaving:u-a:b"},
			want:         registry.Registry{Groups: []registry.Group{}},
			wantProblems: []string{"mate:leaving:", "mate:leaving:u-a:b"},
		},
		{
			name: "two leaving marks for one person are one leaver",
			tags: []string{"mate:leaving:u-gone", "mate:leaving:u-gone"},
			want: registry.Registry{Groups: []registry.Group{}, Leaving: []string{"u-gone"}},
		},
		{
			name: "tag order never matters: a membership before its group",
			tags: []string{"mate:gm:g-a:p-1:mate", "mate:gn:g-a:acme"},
			want: registry.Registry{Groups: []registry.Group{
				{ID: "g-a", Slug: "acme", Projects: []roles.Project{{ID: "p-1", Kind: roles.KindMate}}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, problems := registry.Parse(tc.tags)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse()\n got: %+v\nwant: %+v", got, tc.want)
			}
			var tags []string
			for _, p := range problems {
				tags = append(tags, p.Tag)
				if p.Reason == "" {
					t.Errorf("problem %q has no reason", p.Tag)
				}
			}
			if !reflect.DeepEqual(tags, tc.wantProblems) {
				t.Errorf("problems = %v, want %v (full: %v)", tags, tc.wantProblems, problems)
			}
		})
	}
}

func TestTagsIsTheInverse(t *testing.T) {
	tags := []string{
		"mate:gn:g-acme:acme",
		"mate:gm:g-acme:p-fen:mate",
		"mate:gm:g-acme:p-prod:production",
		"mate:release:g-acme:mates",
		"mate:gn:g-beta:beta",
		"mate:leaving:u-gone",
	}
	parsed, problems := registry.Parse(tags)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	written := registry.Tags(parsed)
	reparsed, problems := registry.Parse(written)
	if len(problems) != 0 {
		t.Fatalf("re-parse problems = %v", problems)
	}
	if !reflect.DeepEqual(parsed, reparsed) {
		t.Errorf("round trip changed the registry\n got: %+v\nwant: %+v", reparsed, parsed)
	}

	// Sorted, so a rewrite that changes nothing writes the same bytes.
	for i := 1; i < len(written); i++ {
		if written[i-1] > written[i] {
			t.Fatalf("tags are not sorted: %v", written)
		}
	}
	if len(written) != len(tags) {
		t.Errorf("wrote %d tags for %d", len(written), len(tags))
	}
}

func TestIsRegistryTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"mate:gn:g:s", true},
		{"mate:gm:g:p:mate", true},
		{"mate:leaving:u", true},
		{"mate:release:g:mates", true},
		{"mate:tool:gitea", false},
		{"mate:g:g-acme", false},
		{"mate:bot:Fen", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := registry.IsRegistryTag(tc.tag); got != tc.want {
			t.Errorf("IsRegistryTag(%q) = %v", tc.tag, got)
		}
	}
}

func TestLookups(t *testing.T) {
	r, _ := registry.Parse([]string{
		"mate:gn:g-acme:acme",
		"mate:gm:g-acme:p-fen:mate",
		"mate:gm:g-acme:p-prod:production",
	})

	if g, ok := r.Group("acme"); !ok || g.ID != "g-acme" {
		t.Errorf("Group(acme) = %+v, %v", g, ok)
	}
	if _, ok := r.Group("nope"); ok {
		t.Error("Group(nope) found something")
	}

	g, kind, ok := r.GroupOfProject("p-fen")
	if !ok || g.Slug != "acme" || kind != roles.KindMate {
		t.Errorf("GroupOfProject(p-fen) = %+v, %q, %v", g, kind, ok)
	}
	if _, _, ok := r.GroupOfProject("p-unknown"); ok {
		t.Error("GroupOfProject(p-unknown) found something")
	}

	prod, ok := g.Production()
	if !ok || prod.ID != "p-prod" {
		t.Errorf("Production() = %+v, %v", prod, ok)
	}
}

// The registry feeds the role function, so the projection has to carry exactly
// what docs/roles.md's input names — and nothing else.
func TestRolesProjection(t *testing.T) {
	r, _ := registry.Parse([]string{
		"mate:gn:g-acme:acme",
		"mate:gm:g-acme:p-fen:mate",
		"mate:gm:g-acme:p-prod:production",
		"mate:release:g-acme:mates",
	})
	got := r.Roles()
	want := roles.Registry{Groups: []roles.Group{{
		ID: "g-acme", Slug: "acme",
		Projects: []roles.Project{
			{ID: "p-fen", Kind: roles.KindMate},
			{ID: "p-prod", Kind: roles.KindProduction},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Roles()\n got: %+v\nwant: %+v", got, want)
	}
}
