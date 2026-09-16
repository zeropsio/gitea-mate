// Package registry reads and writes the group registry: the tags on the org's
// Gitea project (docs/vocabulary.md, "Registry").
//
// Parsing is pure and total. A tag it does not know is ignored — the project
// carries other tags, mate:tool:gitea among them — and a tag that claims to be
// a registry entry but is not shaped like one is reported, never guessed at.
// Nothing here talks to a network.
package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zeropsio/gitea-mate/internal/roles"
)

// The four registry prefixes. Anything else on the project is not ours.
const (
	prefixGroup   = "mate:gn:"
	prefixMember  = "mate:gm:"
	prefixLeaving = "mate:leaving:"
	prefixRelease = "mate:release:"
)

// SlugPattern is the Gitea org name a group registers under: lower-case, 2–30
// characters, starting with a letter (docs/vocabulary.md).
var SlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,29}$`)

// Group is one registered group.
type Group struct {
	ID   string
	Slug string
	// Projects are the group's Zerops projects, sorted by id.
	Projects []roles.Project
	// MatesMayRelease is D8's per-group switch: mate:release:{groupId}:mates.
	MatesMayRelease bool
}

// Production returns the group's production project, if it has one.
func (g Group) Production() (roles.Project, bool) {
	for _, p := range g.Projects {
		if p.Kind == roles.KindProduction {
			return p, true
		}
	}
	return roles.Project{}, false
}

// Registry is the whole registry: groups sorted by slug, plus the people a
// removal is in flight for.
type Registry struct {
	Groups []Group
	// Leaving are the Zerops user ids of members being removed; the app's
	// projects-screen reconcile finishes the removal.
	Leaving []string
}

// Roles projects the registry onto what the role function takes.
func (r Registry) Roles() roles.Registry {
	out := roles.Registry{Groups: make([]roles.Group, 0, len(r.Groups))}
	for _, g := range r.Groups {
		out.Groups = append(out.Groups, roles.Group{ID: g.ID, Slug: g.Slug, Projects: g.Projects})
	}
	return out
}

// Group finds a group by slug.
func (r Registry) Group(slug string) (Group, bool) {
	for _, g := range r.Groups {
		if g.Slug == slug {
			return g, true
		}
	}
	return Group{}, false
}

// GroupOfProject finds the group a Zerops project belongs to, and its kind
// there.
func (r Registry) GroupOfProject(projectID string) (Group, roles.Kind, bool) {
	for _, g := range r.Groups {
		for _, p := range g.Projects {
			if p.ID == projectID {
				return g, p.Kind, true
			}
		}
	}
	return Group{}, "", false
}

// Problem is one tag the registry could not take, and why. A pass reports its
// problems; it never silently drops an entry.
type Problem struct {
	Tag    string
	Reason string
}

func (p Problem) String() string { return p.Tag + ": " + p.Reason }

// Parse reads a project's tag list. It always returns a usable registry: the
// entries it could not take are in the problems, and everything else stands.
func Parse(tags []string) (Registry, []Problem) {
	var problems []Problem
	bad := func(tag, reason string) { problems = append(problems, Problem{Tag: tag, Reason: reason}) }

	groups := map[string]*Group{} // group id -> group
	slugs := map[string]string{}  // slug -> group id

	// Pass one: the groups. Tags are unordered, so a membership cannot be
	// placed until every group is known.
	for _, tag := range tags {
		if !strings.HasPrefix(tag, prefixGroup) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(tag, prefixGroup), ":")
		if len(parts) != 2 || parts[0] == "" {
			bad(tag, "a group tag is mate:gn:{groupId}:{slug}")
			continue
		}
		id, slug := parts[0], parts[1]
		if !SlugPattern.MatchString(slug) {
			bad(tag, fmt.Sprintf("%q is not a slug (%s)", slug, SlugPattern))
			continue
		}
		if other, taken := slugs[slug]; taken && other != id {
			bad(tag, "the slug "+slug+" already belongs to group "+other)
			continue
		}
		if existing, dup := groups[id]; dup {
			if existing.Slug != slug {
				bad(tag, "group "+id+" already registered the slug "+existing.Slug)
			}
			continue
		}
		groups[id] = &Group{ID: id, Slug: slug}
		slugs[slug] = id
	}

	// Pass two: memberships, the release switch and the leavers.
	var leaving []string
	seenLeaving := map[string]bool{}
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, prefixMember):
			parts := strings.Split(strings.TrimPrefix(tag, prefixMember), ":")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
				bad(tag, "a membership tag is mate:gm:{groupId}:{projectId}:{kind}")
				continue
			}
			groupID, projectID, kind := parts[0], parts[1], roles.Kind(parts[2])
			switch kind {
			case roles.KindMate, roles.KindStage, roles.KindProduction:
			default:
				bad(tag, "kind must be mate, stage or production")
				continue
			}
			g, known := groups[groupID]
			if !known {
				bad(tag, "no mate:gn: tag registers group "+groupID)
				continue
			}
			if dup, already := indexOfProject(g.Projects, projectID); already {
				if g.Projects[dup].Kind != kind {
					bad(tag, "project "+projectID+" is already this group's "+string(g.Projects[dup].Kind))
				}
				continue
			}
			if kind == roles.KindProduction {
				if prod, has := g.Production(); has {
					bad(tag, "group "+g.Slug+" already has the production project "+prod.ID)
					continue
				}
			}
			g.Projects = append(g.Projects, roles.Project{ID: projectID, Kind: kind})

		case strings.HasPrefix(tag, prefixRelease):
			parts := strings.Split(strings.TrimPrefix(tag, prefixRelease), ":")
			if len(parts) != 2 || parts[1] != "mates" {
				bad(tag, "a release switch is mate:release:{groupId}:mates")
				continue
			}
			g, known := groups[parts[0]]
			if !known {
				bad(tag, "no mate:gn: tag registers group "+parts[0])
				continue
			}
			g.MatesMayRelease = true

		case strings.HasPrefix(tag, prefixLeaving):
			userID := strings.TrimPrefix(tag, prefixLeaving)
			if userID == "" || strings.Contains(userID, ":") {
				bad(tag, "a leaving mark is mate:leaving:{userId}")
				continue
			}
			if !seenLeaving[userID] {
				seenLeaving[userID] = true
				leaving = append(leaving, userID)
			}
		}
	}

	out := Registry{Groups: make([]Group, 0, len(groups)), Leaving: leaving}
	for _, g := range groups {
		sort.Slice(g.Projects, func(i, j int) bool { return g.Projects[i].ID < g.Projects[j].ID })
		out.Groups = append(out.Groups, *g)
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Slug < out.Groups[j].Slug })
	sort.Strings(out.Leaving)
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Tag != problems[j].Tag {
			return problems[i].Tag < problems[j].Tag
		}
		return problems[i].Reason < problems[j].Reason
	})
	return out, problems
}

// Tags is the inverse: the tag list a registry writes onto the Gitea project,
// sorted, so a rewrite that changes nothing produces a byte-identical list.
// It emits only registry tags; whoever writes them keeps the project's others.
func Tags(r Registry) []string {
	var out []string
	for _, g := range r.Groups {
		out = append(out, prefixGroup+g.ID+":"+g.Slug)
		for _, p := range g.Projects {
			out = append(out, prefixMember+g.ID+":"+p.ID+":"+string(p.Kind))
		}
		if g.MatesMayRelease {
			out = append(out, prefixRelease+g.ID+":mates")
		}
	}
	for _, userID := range r.Leaving {
		out = append(out, prefixLeaving+userID)
	}
	sort.Strings(out)
	return out
}

// IsRegistryTag reports whether a tag belongs to the registry — what a writer
// strips before putting a fresh registry back on a project.
func IsRegistryTag(tag string) bool {
	return strings.HasPrefix(tag, prefixGroup) ||
		strings.HasPrefix(tag, prefixMember) ||
		strings.HasPrefix(tag, prefixLeaving) ||
		strings.HasPrefix(tag, prefixRelease)
}

func indexOfProject(projects []roles.Project, id string) (int, bool) {
	for i, p := range projects {
		if p.ID == id {
			return i, true
		}
	}
	return 0, false
}
