package muse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinVersion is the oldest Muse this adapter supports. 1.4.0 is the first whose
// onFailure successor also runs on an answer Muse rejects, which is what turns
// a crashed or malformed gate into a denial rather than a silent allow.
var MinVersion = Version{Major: 1, Minor: 4}

// TestedBelow is the first version nobody has tested this adapter against.
// Every release from it on is supported with a warning, never refused.
var TestedBelow = Version{Major: 1, Minor: 5}

const versionTimeout = 2 * time.Second

// Version is a Muse release number. A prerelease sorts below the release it
// precedes (semver), so 1.4.0-rc.1 is older than 1.4.0.
type Version struct {
	Major, Minor, Patch int
	// Pre is the prerelease suffix without its dash ("rc.1"), "" for a release.
	Pre string
}

func (v Version) String() string {
	if v.Pre != "" {
		return fmt.Sprintf("%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Pre)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v is an older release than o, by semver precedence: the
// numeric core first, then a release over any of its prereleases, then the
// prerelease identifiers left to right (numeric ones numerically, a numeric
// identifier below an alphanumeric one, a shorter list below a longer one it
// prefixes). So rc.2 is older than rc.10.
//
// 1.4.0-rc.N is below the 1.4.0 floor and is refused on purpose: a release
// candidate is not the build whose failure handling the floor was chosen for.
func (v Version) Less(o Version) bool {
	if c := v.compareCore(o); c != 0 {
		return c < 0
	}
	return comparePre(v.Pre, o.Pre) < 0
}

func (v Version) compareCore(o Version) int {
	for _, p := range [3][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// comparePre orders two prerelease strings; "" (a release) is above any other.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

func compareIdentifier(a, b string) int {
	an, aok := numericIdentifier(a)
	bn, bok := numericIdentifier(b)
	switch {
	case aok && bok:
		return an.Cmp(bn)
	case aok:
		return -1
	case bok:
		return 1
	}
	return strings.Compare(a, b)
}

func numericIdentifier(s string) (*big.Int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return n, ok
}

// gitDescribeSuffix is what `git describe` appends to a tag for a build made
// after it: commits since the tag and an abbreviated hash. It is build
// metadata, not a prerelease: 1.4.1-3-gabc1234 is newer than 1.4.1.
var gitDescribeSuffix = regexp.MustCompile(`^\d+-g[0-9a-f]{4,40}(?:-dirty)?$`)

var semverPattern = regexp.MustCompile(`v?(\d{1,6})\.(\d{1,6})\.(\d{1,6})(?:-([0-9A-Za-z.-]+))?`)

// ParseVersion reads the first major.minor.patch token in `muse --version`'s
// output, which may carry a product name or a commit hash around it.
func ParseVersion(out string) (Version, error) {
	m := semverPattern.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("no version number in %q", truncateForError(out))
	}
	var n [3]int
	for i := range n {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("bad version number in %q: %w", truncateForError(out), err)
		}
		n[i] = v
	}
	pre := m[4]
	if gitDescribeSuffix.MatchString(pre) {
		pre = ""
	}
	return Version{n[0], n[1], n[2], pre}, nil
}

func truncateForError(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// VersionState classifies what `muse --version` said.
type VersionState int

const (
	// VersionSupported is inside the tested range.
	VersionSupported VersionState = iota
	// VersionNotOnPath means there is no muse to ask.
	VersionNotOnPath
	// VersionUnreadable means muse ran (or hung) and no version came of it.
	VersionUnreadable
	// VersionTooOld is below MinVersion: the only state that refuses.
	VersionTooOld
	// VersionUntested is at or above TestedBelow.
	VersionUntested
)

// VersionCheck is the outcome of asking muse for its version.
type VersionCheck struct {
	State   VersionState
	Version Version
	// Detail says why a state other than VersionSupported was chosen.
	Detail string
}

// CheckVersion runs `muse --version` under a 2 second bound. A nil run uses the
// real binary.
func CheckVersion(run Runner) VersionCheck {
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	res, err := run(ctx, "", "--version")
	switch {
	case errors.Is(err, ErrNotOnPath):
		return VersionCheck{State: VersionNotOnPath, Detail: err.Error()}
	case err != nil:
		return VersionCheck{State: VersionUnreadable, Detail: fmt.Sprintf("`muse --version` did not finish: %v", err)}
	case res.ExitCode != 0:
		return VersionCheck{State: VersionUnreadable, Detail: fmt.Sprintf("`muse --version` exited %d", res.ExitCode)}
	}
	v, perr := ParseVersion(string(res.Stdout) + " " + string(res.Stderr))
	if perr != nil {
		return VersionCheck{State: VersionUnreadable, Detail: perr.Error()}
	}
	switch {
	case v.Less(MinVersion):
		return VersionCheck{State: VersionTooOld, Version: v,
			Detail: fmt.Sprintf("muse %s is older than the supported minimum %s", v, MinVersion)}
	case v.compareCore(TestedBelow) >= 0:
		// By numeric core, not Less: 1.5.0-dev sorts below 1.5.0 but is a build
		// of the release nobody has tested against.
		return VersionCheck{State: VersionUntested, Version: v,
			Detail: fmt.Sprintf("muse %s is newer than the range this adapter was tested on (below %s)", v, TestedBelow)}
	}
	return VersionCheck{State: VersionSupported, Version: v}
}
