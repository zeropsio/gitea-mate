package zerops

import "strings"

// ShortShaLength is how much of a commit sha an app version's name carries:
// the seven hex characters git and the Mate app show.
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
//   - one token is the bare full sha of an old stage deploy;
//   - three or more are an old production deploy, "{sha} {tag} {tagger}";
//   - exactly two are a new name, "{label} {short sha}".
//
// Anything else — an empty name, a doubled or stray space, two words whose
// last is not a sha — was named by hand and is not ours: it has none.
func VersionSha(name string) string {
	tokens := strings.Split(name, " ")
	for _, token := range tokens {
		if token == "" {
			return ""
		}
	}
	switch len(tokens) {
	case 1:
		return tokens[0]
	case 2:
		if sha := tokens[1]; len(sha) >= ShortShaLength && isHex(sha) {
			return sha
		}
		return ""
	default:
		return tokens[0]
	}
}

// SameCommit reports whether the sha a version's name spells is the full sha
// of a commit: equal, or a hex prefix of at least [ShortShaLength] characters
// of a full 40-hex sha. A shorter or non-hex token — a dirty working tree's
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
		len(sha) == 40 && isHex(sha) && strings.HasPrefix(sha, token)
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
