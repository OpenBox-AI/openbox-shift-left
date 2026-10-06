package git

import (
	"fmt"
	"strings"
)

// RevParse resolves a revision to its full sha.
func (g Git) RevParse(rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := g.run("rev-parse", rev)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %w", rev, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("rev-parse %s returned nothing", rev)
	}
	return sha, nil
}

// CommitIdentity returns the tree and parent shas for a commit, the fields
// that make a commit event about this exact content rather than about a sha
// string.
func (g Git) CommitIdentity(rev string) (treeSHA string, parents []string, err error) {
	if rev == "" {
		rev = "HEAD"
	}
	out, err := g.run("show", "-s", "--format=%T %P", rev)
	if err != nil {
		return "", nil, fmt.Errorf("read commit identity: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("no commit identity for %s", rev)
	}
	return fields[0], fields[1:], nil
}

// CanonicalRemote returns a host/path identity for the repo's origin, or ""
// when there is no usable remote.
func (g Git) CanonicalRemote() string {
	out, err := g.run("config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return canonicalRemote(strings.TrimSpace(string(out)))
}

func canonicalRemote(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "git@"):
		u = strings.TrimPrefix(u, "git@")
		return strings.Replace(u, ":", "/", 1)
	case strings.Contains(u, "://"):
		parts := strings.SplitN(u, "://", 2)
		rest := parts[1]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return rest
	default:
		return u
	}
}
