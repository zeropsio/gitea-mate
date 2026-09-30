package zerops

import "strings"

// ShortShaLength is the sha a new app version's name spells: the seven hex
// characters git and the Mate app show.
const ShortShaLength = 7

// VersionName is what the broker calls an app version (docs/group-repo.md):
// the label a person reads it by — a stage's branch, production's release tag
// — and the commit's short sha, "v0.1.0 7e2d4c1". Branches and tags cannot
// hold a space, so the name is always exactly two tokens.
func VersionName(label, sha string) string {
	if len(sha) > ShortShaLength {
		sha = sha[:ShortShaLength]
	}
	return label + " " + sha
}

// VersionSha is the commit an app version's name was built from, as far as the
// name spells it — whole or short; [SameCommit] compares it. It reads every
// name the broker has ever written, because services keep the old ones:
//
//   - one token is the bare whole sha of an old stage deploy;
//   - three or more are an old production deploy, "{sha} {tag} {tagger}",
//     whose tagger — a display name — may be empty or hold spaces;
//   - exactly two are a new name, "{label} {short sha}", the sha exactly
//     [ShortShaLength] hex; or an old production name whose tagger was empty.
//
// A whole sha is 40 hex, or 64 in a SHA-256 repository. Anything else — an
// empty name, a stray space, "release 20260930", zcp's "{branch} {sha}-dirty"
// of a working tree with uncommitted changes — was named by hand and is not
// ours: it has none.
func VersionSha(name string) string {
	tokens := strings.Split(name, " ")
	if len(tokens) >= 3 {
		if isWholeSha(tokens[0]) {
			return tokens[0]
		}
		return ""
	}
	for _, token := range tokens {
		if token == "" {
			return ""
		}
	}
	switch {
	case len(tokens) == 1 && isWholeSha(tokens[0]):
		return tokens[0]
	case len(tokens) == 2 && len(tokens[1]) == ShortShaLength && isHex(tokens[1]):
		return tokens[1]
	case len(tokens) == 2 && isWholeSha(tokens[0]):
		return tokens[0]
	}
	return ""
}

// SameCommit reports whether the sha a version's name spells is the full sha
// of a commit: equal, or a hex prefix of at least [ShortShaLength] characters
// of a whole sha (40 hex, or 64). A shorter or non-hex token — a dirty working tree's
// "{sha}-dirty" among them — never matches a commit it only begins like, and
// a short sha on the right is a spelling nothing can check.
func SameCommit(token, sha string) bool {
	if token == "" {
		return false
	}
	if token == sha {
		return true
	}
	return len(token) >= ShortShaLength && isHex(token) &&
		isWholeSha(sha) && strings.HasPrefix(sha, token)
}

// isWholeSha reports a whole commit sha: 40 hex, or 64 in a SHA-256
// repository.
func isWholeSha(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHex(s)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
