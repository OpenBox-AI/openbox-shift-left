package devconfig

import (
	"os"
	"sync"
	"time"
)

// Resolving one boolean touches four files: the managed config and its raw key
// set, then the user config and its raw key set. An enforce-mode hook resolves
// eight or so flags before it decides, so a single tool call re-read and
// re-parsed the same two files roughly thirty times.
//
// The cost was not really the I/O — it was that each flag saw its own read.
// Enforce, FailClosed and Tier2 could resolve against three different versions
// of dev.json if the file were rewritten mid-hook (by `init`, or an org
// config push), producing a gate decision assembled from a posture that never
// existed as a whole.
//
// So reads are cached, keyed by the file's identity rather than for the process
// lifetime: a snapshot is reused only while the path, size and modification
// time are unchanged. A hook resolving eight flags in microseconds gets one
// read and one coherent view. A test — or a developer — that rewrites the file
// gets the new content on the next resolve, with no cache to invalidate by hand
// and no stale-read trap for whoever writes the next test.
type fileCache[T any] struct {
	mu    sync.Mutex
	stamp fileStamp
	val   T
	ok    bool
}

// fileStamp identifies a file version cheaply enough to check on every resolve.
type fileStamp struct {
	path    string
	size    int64
	modTime time.Time
	missing bool
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{path: path, missing: true}
	}
	return fileStamp{path: path, size: fi.Size(), modTime: fi.ModTime()}
}

// get returns the cached value for path, calling load only when the file's
// stamp has changed since the last call.
func (c *fileCache[T]) get(path string, load func() T) T {
	s := stampOf(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.stamp == s {
		return c.val
	}
	c.val = load()
	c.stamp = s
	c.ok = true
	return c.val
}

var (
	managedCache fileCache[managedState]
	userCache    fileCache[userState]
	// readCount counts underlying file loads, so a test can assert that
	// resolving many flags does not mean many reads.
	readCount struct {
		mu sync.Mutex
		n  int
	}
)

// userState is the user config plus the raw key set that tells "absent" from
// "explicitly false" — the two things every resolve needs from that file.
type userState struct {
	cfg  DevConfig
	keys map[string]bool
	err  error
}

func countRead() {
	readCount.mu.Lock()
	readCount.n++
	readCount.mu.Unlock()
}

// ReadCount reports how many times a config file has actually been loaded.
// Exposed for tests that assert the per-hook read budget.
func ReadCount() int {
	readCount.mu.Lock()
	defer readCount.mu.Unlock()
	return readCount.n
}

func cachedManaged() managedState {
	pinned.mu.Lock()
	if pinned.depth > 0 && pinned.managed != nil {
		defer pinned.mu.Unlock()
		return *pinned.managed
	}
	pinned.mu.Unlock()

	st := managedCache.get(ManagedConfigPath(), func() managedState {
		countRead()
		return loadManagedUncached()
	})

	pinned.mu.Lock()
	if pinned.depth > 0 && pinned.managed == nil {
		pinned.managed = &st
	}
	pinned.mu.Unlock()
	return st
}

func cachedUser() userState {
	pinned.mu.Lock()
	if pinned.depth > 0 && pinned.user != nil {
		defer pinned.mu.Unlock()
		return *pinned.user
	}
	pinned.mu.Unlock()

	path := DefaultConfigPath()
	st := userCache.get(path, func() userState {
		countRead()
		cfg, err := Load(path)
		return userState{cfg: cfg, keys: configKeysUncached(path), err: err}
	})

	pinned.mu.Lock()
	if pinned.depth > 0 && pinned.user == nil {
		pinned.user = &st
	}
	pinned.mu.Unlock()
	return st
}

// pinned holds the config view frozen by Pin. Stat-keyed caching alone does not
// give a gate decision a consistent view: if dev.json is genuinely rewritten
// mid-hook the stamp changes and the next flag correctly sees the new file —
// correct in isolation, but it means Enforce, FailClosed and Tier2 can come
// from different versions and produce a posture that never existed as a whole.
var pinned struct {
	mu      sync.Mutex
	depth   int
	managed *managedState
	user    *userState
}

// Pin freezes config reads until the returned function is called, so everything
// decided inside one gate sees one version of the file.
//
// A hook is a short-lived, single-purpose process, so the natural scope is the
// whole run; it is explicit rather than implicit because a long-lived caller
// must not silently pin a posture forever. Nested pins are counted, and the
// outermost release clears the freeze. Reads outside a pin fall back to the
// stat-keyed cache, which is why tests observe rewrites without ceremony.
func Pin() (release func()) {
	pinned.mu.Lock()
	pinned.depth++
	pinned.mu.Unlock()
	return func() {
		pinned.mu.Lock()
		defer pinned.mu.Unlock()
		if pinned.depth--; pinned.depth <= 0 {
			pinned.depth = 0
			pinned.managed, pinned.user = nil, nil
		}
	}
}
