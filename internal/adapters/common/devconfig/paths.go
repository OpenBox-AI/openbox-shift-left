package devconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// EnvHome relocates the whole OpenBox config directory.
const EnvHome = "OPENBOX_HOME"

// Home is the OpenBox config directory: $OPENBOX_HOME when set, else
// $HOME/.openbox. It never creates the directory; a read path must be able to
// ask where a file would be without making anything on disk. EnsureHome does
// the creating, and only write paths call it.
func Home() (string, error) {
	if p := os.Getenv(EnvHome); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path (got %q)", EnvHome, p)
		}
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot locate a home directory for the OpenBox config dir: set %s to an absolute path", EnvHome)
	}
	return filepath.Join(home, ".openbox"), nil
}

func ensureHome() (string, error) {
	dir, err := Home()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// bound is the tool this process is acting for. Each governed tool carries its
// own agent identity under ~/.openbox/<tool>/, so every identity path has to
// know which tool is asking -- and the processes that ask (a hook, a rewake, an
// installer, a lane daemon) each act for exactly one tool for their whole life.
// Binding once at dispatch reaches ResolveDID, ResolveCredentials,
// ResolveCoordinates and every other resolver without threading a provider
// argument through each of them.
//
// The zero value means unbound, which resolves the org-level files -- what
// `auth`, the legacy-config migration and the enumerating commands want.
var bound struct {
	mu   sync.RWMutex
	name string
}

// BindProvider binds this process to a tool's identity store and returns a
// closure restoring the previous binding. Production binds once per process;
// the restore closure exists for a test binary, which dispatches many commands
// in one process and would otherwise let the first bind decide every later
// case's identity.
//
// The name becomes a directory component under ~/.openbox, so validation here
// is the whole defence against a traversal: this is the only writer of `bound`.
// It cannot check the name against provider.Supported() -- internal/provider
// imports this package -- so it validates the shape and a cmd/openbox test
// cross-checks the set.
func BindProvider(name string) (release func(), err error) {
	if err := validateProviderName(name); err != nil {
		return nil, err
	}
	bound.mu.Lock()
	previous := bound.name
	bound.name = name
	bound.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			bound.mu.Lock()
			bound.name = previous
			bound.mu.Unlock()
		})
	}, nil
}

// BoundProvider reports the tool this process is acting for, or "" when
// nothing is bound.
func BoundProvider() string {
	bound.mu.RLock()
	defer bound.mu.RUnlock()
	return bound.name
}

// validateProviderName rejects anything that would not survive being joined
// onto Home() as exactly one directory element. A separator, a dot element or
// surrounding whitespace would each put a credential somewhere other than the
// store it claims to be.
func validateProviderName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("provider name is empty; an identity store needs a tool to belong to")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("provider name %q has surrounding whitespace", name)
	case strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf("provider name %q contains a path separator; it must be one directory element", name)
	case name == "." || name == "..":
		return fmt.Errorf("provider name %q is a relative path element", name)
	case filepath.VolumeName(name) != "":
		return fmt.Errorf("provider name %q carries a volume name", name)
	case filepath.Base(name) != name || filepath.Clean(name) != name:
		return fmt.Errorf("provider name %q is not a single clean path element", name)
	}
	return nil
}

// IdentityDirFor exposes one tool's identity directory, for the one caller
// that needs the directory rather than a file in it: `uninstall`, which
// removes each store's directory once it has deleted the files by name.
func IdentityDirFor(tool string) (string, error) { return identityDirFor(tool) }

// identityDirFor is the one join site for every per-tool identity path: the
// org directory when tool is empty, else ~/.openbox/<tool>. It re-validates
// because the explicit *For accessors take a caller-supplied name that never
// passed through BindProvider.
func identityDirFor(tool string) (string, error) {
	dir, err := Home()
	if err != nil {
		return "", err
	}
	if tool == "" {
		return dir, nil
	}
	if err := validateProviderName(tool); err != nil {
		return "", err
	}
	return filepath.Join(dir, tool), nil
}

// ErrProviderUnbound is returned by EnvFilePath when nothing is bound.
var ErrProviderUnbound = errors.New("no provider bound: this process has not said which tool it is acting for")

// EnvFilePath is the bound tool's credential file, ~/.openbox/<tool>/.env.
// Note what is deliberately absent: OPENBOX_CONFIG names a dev.json and has
// never shadowed this file, which is why a per-tool .env stays per-tool even
// under an operator override.
//
// Unbound it is an error, and that is a security control rather than
// tidiness. Without it, any command that forgets to bind reads the org-level
// .env -- a file that holds a credential authorizing agent creation across the
// whole organization and no agent identity at all. Silently reading it is
// exactly the cross-boundary read the per-tool split exists to make
// impossible, and "returns something plausible" is how that bug would ship.
//
// A caller that legitimately wants a specific file says so: OrgEnvFilePath for
// the org one, EnvFilePathFor(tool) for a named tool's. Never relax this to
// make a test pass; bind, or name the file.
func EnvFilePath() (string, error) {
	tool := BoundProvider()
	if tool == "" {
		return "", ErrProviderUnbound
	}
	return EnvFilePathFor(tool)
}

// OrgEnvFilePath is always ~/.openbox/.env, whatever is bound. The org control
// token lives here and nowhere else, so that a compromised tool store is not a
// fleet compromise; `uninstall` purges this file by name.
func OrgEnvFilePath() (string, error) { return EnvFilePathFor("") }

// EnvFilePathFor names one tool's credential file without binding. The
// enumerating commands (doctor, uninstall) read every store through this, so
// that they never bind inside a loop -- a bind held across a cached read is the
// one way those loops could report the first store's identity for every row.
func EnvFilePathFor(tool string) (string, error) {
	dir, err := identityDirFor(tool)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env"), nil
}

// workloadTokenCacheFileName is the v3 bearer-token cache's file name under a
// tool's identity directory. A cache, not a store (see the package doc): it is
// never a source for any field other than itself, and WriteWorkloadIdentity
// deletes it on every identity write to prevent a cross-identity bearer reuse.
const workloadTokenCacheFileName = "workload-token.json"

// WorkloadTokenCachePathFor names one tool's workload-token cache without
// binding, the same shape as EnvFilePathFor/DevConfigPathFor.
func WorkloadTokenCachePathFor(tool string) (string, error) {
	dir, err := identityDirFor(tool)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, workloadTokenCacheFileName), nil
}

// WorkloadTokenCachePath is the bound form of WorkloadTokenCachePathFor,
// refusing when nothing is bound for the same reason EnvFilePath does: an
// unbound caller has no tool-scoped answer to give.
func WorkloadTokenCachePath() (string, error) {
	tool := BoundProvider()
	if tool == "" {
		return "", ErrProviderUnbound
	}
	return WorkloadTokenCachePathFor(tool)
}

// DevConfigPath is where to read the dev config: $OPENBOX_CONFIG when set,
// else the bound tool's ~/.openbox/<tool>/dev.json. Unbound it is
// ~/.openbox/dev.json, falling back to the legacy location while an unmigrated
// file still lives there (see resolveConfigPath) -- a bound read has no legacy
// location to fall back to, because a per-tool store has never existed
// anywhere else.
func DevConfigPath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	if tool := BoundProvider(); tool != "" {
		return DevConfigPathFor(tool)
	}
	return resolveConfigPath("dev.json")
}

// InstallConfigPath is the dev config an installer writes: override when an
// installer was handed one, else DevConfigWritePath, else DefaultConfigPath.
func InstallConfigPath(override string) string {
	if override != "" {
		return override
	}
	if p, err := DevConfigWritePath(); err == nil {
		return p
	}
	return DefaultConfigPath()
}

// DevConfigWritePath is where to write the dev config; always the new
// location, never the legacy one, so a write can never re-entrench the old
// path. Callers that write must run MigrateLegacyConfig first.
func DevConfigWritePath() (string, error) {
	if p := os.Getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	return DevConfigWritePathFor(BoundProvider())
}

// DevConfigPathFor and DevConfigWritePathFor name one tool's dev config
// without binding and without the OPENBOX_CONFIG override: an enumerator
// asking for a specific store wants that store, not whatever the operator
// pointed the ambient resolver at. For a per-tool store the read and write
// paths are the same file; they stay two functions so callers keep reading
// like their unbound counterparts.
func DevConfigPathFor(tool string) (string, error) { return DevConfigWritePathFor(tool) }

func DevConfigWritePathFor(tool string) (string, error) {
	dir, err := identityDirFor(tool)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dev.json"), nil
}

// resolveConfigPath picks between the new location and the legacy one for a
// read, and always returns the new one for a write. This exists because
// upgrading the binary must not silently ungovern a machine.
func resolveConfigPath(name string) (string, error) {
	dir, err := Home()
	if err != nil {
		return filepath.Join(legacyConfigDir(), name), nil
	}
	newPath := filepath.Join(dir, name)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, nil
	}
	legacy := filepath.Join(legacyConfigDir(), name)
	if legacy != newPath {
		if _, err := os.Stat(legacy); err == nil {
			return legacy, nil
		}
	}
	return newPath, nil
}

// userConfigDir is os.UserConfigDir with the $HOME/.config fallback.
func userConfigDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join(os.Getenv("HOME"), ".config")
	}
	return dir
}

// ConfigDir is OpenBox's directory under the user's config base, and the one
// resolver for it. Everything the developer runtime keeps there -- the spool,
// the findings cursor, the legacy config, the session registry, the pending
// approvals, the halted sessions, the enforcement and advisory sinks -- goes
// through here, because the fallback above is a rule about one machine: a
// second copy of it that changes (an XDG_CONFIG_HOME reading, a Windows-shaped
// fallback) puts two of those sinks on different bases with nothing to notice.
func ConfigDir() string {
	return filepath.Join(userConfigDir(), "openbox")
}

func legacyConfigDir() string {
	return ConfigDir()
}
