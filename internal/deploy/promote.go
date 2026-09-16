// Package deploy is what the broker does with a commit: decide, queue and
// perform.
//
// Nothing here executes repository code. A deploy moves bytes — a Gitea commit
// archive, or the app code of a stage version being promoted — from Gitea to
// Zerops, and Zerops builds. The one `git` the broker ever runs is the merge of
// a mixed-source stage, in a throwaway working copy with hooks disabled
// (decide.go).
package deploy

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/zeropsio/gitea-mate/internal/environments"
)

// Choice is how one service's deploy gets its bytes.
type Choice string

// The two ways bytes reach a Zerops app version.
const (
	// Promote reuses the app code a stage version already built from this very
	// commit: no second build, and what production runs is byte-for-byte what
	// stage ran (the measured path, ledger 2026-09-15).
	Promote Choice = "promote"
	// Archive takes the commit's tar.gz from Gitea and uploads it. Zerops
	// builds it.
	Archive Choice = "archive"
)

// PromoteOption is everything the choice depends on, gathered by the executor
// and decided here, so the rule is a table and not a branch buried in a
// network call.
type PromoteOption struct {
	// Tier is the target environment's tier. Only production promotes: a stage
	// is where a commit is built in the first place.
	Tier environments.Tier
	// StageVersionID is the app version a stage built from the same commit,
	// empty when there is none — a commit that never reached a stage, or one
	// whose stage build failed.
	StageVersionID string
	// SameBuild says whether the two tiers' setups share their build section.
	// They may differ — production's setup can run differently — but if they
	// build differently, the stage artifact is not what production wants.
	SameBuild bool
}

// Choose is the rule of guide 5.3.
func (o PromoteOption) Choose() Choice {
	if o.Tier == environments.TierProduction && o.StageVersionID != "" && o.SameBuild {
		return Promote
	}
	return Archive
}

// ---------------------------------------------------------------------------
// zerops.yaml
// ---------------------------------------------------------------------------

// ZeropsYamlNames are the two spellings the platform accepts, in the order the
// broker looks for them.
var ZeropsYamlNames = []string{"zerops.yaml", "zerops.yml"}

// SameBuild reports whether two setups of one zerops.yaml build identically.
// Both must exist: a setup a tier names and the repository does not carry is a
// recipe that cannot be deployed, and saying "not the same" would quietly
// rebuild instead of saying so.
func SameBuild(zeropsYaml []byte, a, b string) (bool, error) {
	setups, err := parseSetups(zeropsYaml)
	if err != nil {
		return false, err
	}
	left, ok := setups[a]
	if !ok {
		return false, fmt.Errorf("zerops.yaml has no setup %q", a)
	}
	right, ok := setups[b]
	if !ok {
		return false, fmt.Errorf("zerops.yaml has no setup %q", b)
	}
	if a == b {
		return true, nil
	}
	return bytes.Equal(left, right), nil
}

// HasSetup reports whether a zerops.yaml carries one.
func HasSetup(zeropsYaml []byte, setup string) bool {
	setups, err := parseSetups(zeropsYaml)
	if err != nil {
		return false
	}
	_, ok := setups[setup]
	return ok
}

// parseSetups returns each setup's build section, canonicalised: decoded and
// re-marshalled, so two sections that differ only in the order their keys were
// written compare equal. A setup with no build section has an empty one, which
// is still a value two setups can share.
func parseSetups(zeropsYaml []byte) (map[string][]byte, error) {
	var doc struct {
		Zerops []struct {
			Setup string    `yaml:"setup"`
			Build yaml.Node `yaml:"build"`
		} `yaml:"zerops"`
	}
	if err := yaml.Unmarshal(zeropsYaml, &doc); err != nil {
		return nil, fmt.Errorf("zerops.yaml: %w", err)
	}
	if len(doc.Zerops) == 0 {
		return nil, fmt.Errorf("zerops.yaml carries no setups")
	}
	out := make(map[string][]byte, len(doc.Zerops))
	for _, entry := range doc.Zerops {
		if entry.Setup == "" {
			return nil, fmt.Errorf("zerops.yaml carries a setup with no name")
		}
		canonical, err := canonicalise(entry.Build)
		if err != nil {
			return nil, fmt.Errorf("zerops.yaml: setup %s: %w", entry.Setup, err)
		}
		out[entry.Setup] = canonical
	}
	return out, nil
}

// canonicalise renders a node through Go values, which yaml.v3 emits with its
// mapping keys sorted.
func canonicalise(node yaml.Node) ([]byte, error) {
	if node.Kind == 0 {
		return []byte{}, nil
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return nil, err
	}
	return yaml.Marshal(value)
}
