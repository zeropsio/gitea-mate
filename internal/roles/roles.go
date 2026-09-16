// Package roles is the Go twin of the shared role function.
//
// The rules are docs/roles.md; the cases are fixtures.json, which is copied
// byte-for-byte into the Mate fork as packages/shared/src/zeropsRoles.fixtures.json
// and replayed by both test suites. A change to the rules is a change to the
// fixture first, in both repositories.
//
// Zerops roles are the only source of rights. Nothing here invents a rule.
package roles

import (
	"sort"
	"strings"
)

// Role is a Zerops role. They rank NO_ACCESS < READ_ONLY < BASIC_USER < ADMIN
// < OWNER; anything unknown ranks with NO_ACCESS, so a role the platform adds
// grants nothing until this file says it does.
type Role string

const (
	NoAccess  Role = "NO_ACCESS"
	ReadOnly  Role = "READ_ONLY"
	BasicUser Role = "BASIC_USER"
	Admin     Role = "ADMIN"
	Owner     Role = "OWNER"
)

func (r Role) rank() int {
	switch r {
	case ReadOnly:
		return 1
	case BasicUser:
		return 2
	case Admin:
		return 3
	case Owner:
		return 4
	default:
		return 0
	}
}

// AtLeast reports whether r ranks at or above other.
func (r Role) AtLeast(other Role) bool { return r.rank() >= other.rank() }

// StatusActive is the one member status that grants anything.
const StatusActive = "ACTIVE"

// Kind is what a project is to its group.
type Kind string

const (
	KindMate       Kind = "mate"
	KindStage      Kind = "stage"
	KindProduction Kind = "production"
)

// Person is one row of the org's member list.
type Person struct {
	ID                string `json:"id"`
	OrgRole           Role   `json:"orgRole"`
	Status            string `json:"status"`
	CanCreateProjects bool   `json:"canCreateProjects"`
}

// Project is one project of a group, as the registry names it.
type Project struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
}

// Group is one registered group.
type Group struct {
	ID       string    `json:"id"`
	Slug     string    `json:"slug"`
	Projects []Project `json:"projects"`
}

// Registry is the whole registry: the groups of docs/vocabulary.md's tags.
type Registry struct {
	Groups []Group `json:"groups"`
}

// Production returns the group's production project, if it has one. A group
// has at most one (docs/vocabulary.md); the first wins if a malformed registry
// ever carries two.
func (g Group) Production() (Project, bool) {
	for _, p := range g.Projects {
		if p.Kind == KindProduction {
			return p, true
		}
	}
	return Project{}, false
}

// GroupRights are the three flags a group grants, which become its Gitea teams.
type GroupRights struct {
	Read    bool `json:"read"`
	Write   bool `json:"write"`
	Release bool `json:"release"`
}

// Mate states: what a person may do with a Mate project.
const (
	MateOpen   = "open"
	MateListed = "listed"
	MateHidden = "hidden"
)

// Rights is the whole answer. mates names every Mate-kind project in the
// registry and groups every group, so a consumer never has to know which
// projects exist to read it.
type Rights struct {
	Active    bool                   `json:"active"`
	SiteAdmin bool                   `json:"siteAdmin"`
	CanCreate bool                   `json:"canCreate"`
	Claims    []string               `json:"claims"`
	Mates     map[string]string      `json:"mates"`
	Groups    map[string]GroupRights `json:"groups"`
}

// Compute is the role function. overrides is the person's per-project role
// where a project carries one (each project's userRoles entry for them).
func Compute(person Person, overrides map[string]Role, registry Registry) Rights {
	active := person.Status == StatusActive

	effective := func(projectID string) Role {
		if !active {
			return NoAccess
		}
		if r, ok := overrides[projectID]; ok {
			return r
		}
		return person.OrgRole
	}

	out := Rights{
		Active:    active,
		SiteAdmin: active && person.OrgRole == Owner,
		CanCreate: active && (person.OrgRole.AtLeast(Admin) || person.CanCreateProjects),
		Claims:    []string{},
		Mates:     map[string]string{},
		Groups:    map[string]GroupRights{},
	}

	for _, g := range registry.Groups {
		var rights GroupRights
		if active {
			for _, p := range g.Projects {
				if effective(p.ID).AtLeast(BasicUser) {
					rights.Write = true
					break
				}
			}
			rights.Read = rights.Write || person.OrgRole.AtLeast(ReadOnly)
			if prod, ok := g.Production(); ok {
				rights.Release = effective(prod.ID).AtLeast(BasicUser)
			} else {
				// Until a group has a production project its releasers are the
				// org's owners and admins — the recipe is merged before
				// production exists.
				rights.Release = person.OrgRole.AtLeast(Admin)
			}
		}
		out.Groups[g.Slug] = rights

		for _, p := range g.Projects {
			if p.Kind != KindMate {
				continue
			}
			switch r := effective(p.ID); {
			case r.AtLeast(BasicUser):
				out.Mates[p.ID] = MateOpen
			case r == ReadOnly:
				out.Mates[p.ID] = MateListed
			default:
				out.Mates[p.ID] = MateHidden
			}
		}
	}

	if out.SiteAdmin {
		out.Claims = append(out.Claims, "org:owner")
	}
	for slug, rights := range out.Groups {
		if rights.Read {
			out.Claims = append(out.Claims, "g:"+slug+":read")
		}
		if rights.Write {
			out.Claims = append(out.Claims, "g:"+slug+":write")
		}
		if rights.Release {
			out.Claims = append(out.Claims, "g:"+slug+":release")
		}
	}
	sort.Strings(out.Claims)
	return out
}

// TeamsFor returns the Gitea teams a person belongs to in one group, in the
// order docs/vocabulary.md lists them. The rights loop writes exactly these.
func (r GroupRights) TeamsFor() []string {
	var teams []string
	for _, t := range []struct {
		name string
		on   bool
	}{{"read", r.Read}, {"write", r.Write}, {"release", r.Release}} {
		if t.on {
			teams = append(teams, t.name)
		}
	}
	return teams
}

// Login is the Gitea login of a Zerops user: "u-" plus the user id,
// lower-cased, every character outside [a-z0-9] dropped
// (docs/vocabulary.md). Deterministic, valid for Gitea, and never an e-mail's
// local part — two people's jan@ would collide.
func Login(zeropsUserID string) string {
	var b strings.Builder
	b.WriteString("u-")
	for _, c := range strings.ToLower(zeropsUserID) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}
