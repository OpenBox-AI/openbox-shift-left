package devconfig

import (
	"os"
	"sync"
	"time"
)

type fileCache[T any] struct {
	mu    sync.Mutex
	stamp fileStamp
	val   T
	ok    bool
}

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
	readCount    struct {
		mu sync.Mutex
		n  int
	}
)

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

var pinned struct {
	mu      sync.Mutex
	depth   int
	managed *managedState
	user    *userState
}

// Pin freezes config reads until the returned function is called, so
// everything decided inside one gate sees one version of the file.
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
