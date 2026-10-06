package workloadauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// refreshSkew is how far before the token's own expiry this package treats
// an entry as stale, so a caller never hands out a token that expires
// mid-flight.
const refreshSkew = 30 * time.Second

// refreshAhead is how far before expiry a token is renewed while it is still
// usable. Inside this window a failed renewal is not an outage: the old token
// keeps being handed out until refreshSkew, so a slow or briefly unreachable
// token endpoint costs a gated hook at most refreshAheadTimeout rather than
// its whole evaluation budget. The lane daemons' warmer (Warm) renews at the
// same threshold, so while a daemon runs a hook rarely renews at all.
const refreshAhead = 120 * time.Second

// refreshAheadTimeout bounds one renewal attempt made while the old token is
// still usable. A cold fetch (no usable token) keeps the caller's own deadline.
const refreshAheadTimeout = 3 * time.Second

// refreshRetryBackoff is how long after a failed early renewal every process
// sharing the cache keeps using the old token without trying again, so a hung
// token endpoint is paid for once per window, not once per hook.
const refreshRetryBackoff = 15 * time.Second

// maxCacheLifetime is the ceiling on how long a fetched token is cached,
// regardless of what the server's expires_in claims: min(expires_in, 300)s.
const maxCacheLifetime = 300 * time.Second

// negativeCacheTTL is how long a bootstrap 401/403/404/409 is remembered,
// so one-process-per-hook does not re-bootstrap on every event.
const negativeCacheTTL = 60 * time.Second

// Entry is the cache's one record: either a live token or a negative
// (bootstrap-unavailable) entry, never both. A negative entry carries no
// token.
type Entry struct {
	AccessToken       string    `json:"access_token,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	Kid               string    `json:"kid,omitempty"`
	TokenEndpoint     string    `json:"token_endpoint,omitempty"`
	ActivationVersion string    `json:"activation_version,omitempty"`
	KeyThumbprint     string    `json:"key_thumbprint,omitempty"`
	NegUntil          time.Time `json:"neg_until,omitempty"`
	NegReason         string    `json:"neg_reason,omitempty"`
	// RefreshRetryAt is set after an early renewal failed: until then the
	// still-usable token is handed out with no network call.
	RefreshRetryAt time.Time `json:"refresh_retry_at,omitempty"`
}

// fresh reports whether e is a usable, live token for thumbprint as of now:
// non-empty, matching key, and more than refreshSkew away from expiry.
func (e Entry) fresh(now time.Time, thumbprint string) bool {
	if e.AccessToken == "" || e.KeyThumbprint != thumbprint {
		return false
	}
	return now.Before(e.ExpiresAt.Add(-refreshSkew))
}

// dueForRefresh reports whether a fresh e is inside the refreshAhead window
// and not held back by a recent failed renewal.
func (e Entry) dueForRefresh(now time.Time) bool {
	if now.Before(e.ExpiresAt.Add(-refreshAhead)) {
		return false
	}
	return e.RefreshRetryAt.IsZero() || !now.Before(e.RefreshRetryAt)
}

// negative reports whether e is a live negative-cache entry as of now.
func (e Entry) negative(now time.Time) bool {
	return !e.NegUntil.IsZero() && now.Before(e.NegUntil)
}

// Cache is the storage seam Authenticator uses: FileCache for the
// one-process-per-hook case, a memory cache for lane daemons and git-action.
type Cache interface {
	// Load returns the stored entry, or (Entry{}, false) for a miss. A
	// corrupt or unreadable backing store is a miss, never an error.
	Load() (Entry, bool)
	Store(Entry) error
	Clear() error
}

// FileCache is a 0600 JSON file per tool, written atomically.
type FileCache struct {
	Path string

	// rename defaults to os.Rename. A test overrides it to observe the
	// directory listing between CreateTemp and Rename -- the one window a
	// residual .workload-token-*.tmp file would be visible in.
	rename func(oldpath, newpath string) error
}

// Load reads and decodes the cache file. Any failure -- missing file,
// permission error, corrupt JSON -- is a miss, never an error: a cache is
// never load-bearing for correctness, only for avoiding an extra round trip.
func (c *FileCache) Load() (Entry, bool) {
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, false
	}
	return e, true
}

// Store writes e atomically: CreateTemp beside the target, Chmod 0600, write,
// Close, Rename. This is NOT renameio: renameio.WriteFile stages its temp
// file in os.TempDir() when that is on the same device, and
// WithExistingPermissions would leave a pre-existing 0644 target at 0644.
// This is the same pattern as
// internal/adapters/common/devconfig/envfile.go's WriteEnvFile, duplicated
// deliberately rather than shared: to find every copy, grep the pattern, not
// the importer.
func (c *FileCache) Store(e Entry) error {
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode cache entry: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".workload-token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp cache file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cache file: %w", err)
	}

	rename := c.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tmpName, c.Path); err != nil {
		return fmt.Errorf("commit %s: %w", c.Path, err)
	}
	return nil
}

// Clear removes the cache file. Removing an already-absent file is not an
// error.
func (c *FileCache) Clear() error {
	if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", c.Path, err)
	}
	return nil
}

// memoryCache is the in-process Cache lane daemons and git-action use: an
// empty cache path selects this instead of a file.
type memoryCache struct {
	mu    sync.Mutex
	entry Entry
	has   bool
}

// NewMemoryCache returns a Cache backed by process memory only.
func NewMemoryCache() Cache { return &memoryCache{} }

func (c *memoryCache) Load() (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entry, c.has
}

func (c *memoryCache) Store(e Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = e
	c.has = true
	return nil
}

func (c *memoryCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = Entry{}
	c.has = false
	return nil
}
