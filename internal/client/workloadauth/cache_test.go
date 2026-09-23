package workloadauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWarmCacheMakesNoRequests(t *testing.T) {
	dir := t.TempDir()
	fc := &FileCache{Path: filepath.Join(dir, "token.json")}
	now := time.Now()
	entry := Entry{
		AccessToken:   "atk",
		ExpiresAt:     now.Add(200 * time.Second),
		KeyThumbprint: "thumb-1",
	}
	if err := fc.Store(entry); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, ok := fc.Load()
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if !got.fresh(now, "thumb-1") {
		t.Fatal("expected the loaded entry to be fresh")
	}
}

func TestCacheUsableLifeIsAtMost270s(t *testing.T) {
	now := time.Now()
	entry := Entry{AccessToken: "atk", KeyThumbprint: "thumb-1", ExpiresAt: now.Add(300 * time.Second)}
	// At 270s left (300 - 30s skew), still fresh with zero margin below.
	if !entry.fresh(now.Add(29*time.Second), "thumb-1") {
		t.Fatal("expected fresh at 271s remaining")
	}
	if entry.fresh(now.Add(271*time.Second), "thumb-1") {
		t.Fatal("expected stale once inside the 30s skew window")
	}
}

func TestCacheMissOnForeignKeyThumbprint(t *testing.T) {
	now := time.Now()
	entry := Entry{AccessToken: "atk", KeyThumbprint: "thumb-1", ExpiresAt: now.Add(200 * time.Second)}
	if entry.fresh(now, "thumb-2") {
		t.Fatal("expected a miss for a foreign key thumbprint")
	}
}

func TestCorruptCacheIsAMiss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing corrupt cache: %v", err)
	}
	fc := &FileCache{Path: path}
	_, ok := fc.Load()
	if ok {
		t.Fatal("expected a corrupt cache file to be a miss, not a hit")
	}
}

func TestUnreadableCacheIsAMiss(t *testing.T) {
	fc := &FileCache{Path: filepath.Join(t.TempDir(), "does-not-exist", "token.json")}
	_, ok := fc.Load()
	if ok {
		t.Fatal("expected a missing cache file to be a miss")
	}
}

func TestCacheTempIsStagedBesideTheTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	fc := &FileCache{Path: path}

	var sawOutsideTargetDir bool
	fc.rename = func(oldpath, newpath string) error {
		entries, err := os.ReadDir(filepath.Dir(oldpath))
		if err != nil {
			t.Fatalf("reading dir mid-write: %v", err)
		}
		if filepath.Dir(oldpath) != dir {
			sawOutsideTargetDir = true
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".workload-token-") {
				continue
			}
			if filepath.Join(filepath.Dir(oldpath), e.Name()) != oldpath {
				t.Errorf("unexpected residue %q alongside the real temp file", e.Name())
			}
		}
		return os.Rename(oldpath, newpath)
	}

	if err := fc.Store(Entry{AccessToken: "atk"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if sawOutsideTargetDir {
		t.Fatal("the temp file was staged outside the target directory")
	}
}

func TestCacheClearRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	fc := &FileCache{Path: path}
	if err := fc.Store(Entry{AccessToken: "atk"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := fc.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the cache file to be gone, stat err = %v", err)
	}
	// Clear on an already-absent file must not error.
	if err := fc.Clear(); err != nil {
		t.Fatalf("Clear on an absent file: %v", err)
	}
}

func TestMemoryCacheRoundTrips(t *testing.T) {
	mc := NewMemoryCache()
	if _, ok := mc.Load(); ok {
		t.Fatal("expected an empty memory cache to miss")
	}
	entry := Entry{AccessToken: "atk", KeyThumbprint: "thumb-1", ExpiresAt: time.Now().Add(time.Minute)}
	if err := mc.Store(entry); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, ok := mc.Load()
	if !ok || got.AccessToken != "atk" {
		t.Fatalf("Load = %+v, %v", got, ok)
	}
	if err := mc.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := mc.Load(); ok {
		t.Fatal("expected a cleared memory cache to miss")
	}
}

func TestEntryJSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second).UTC()
	e := Entry{
		AccessToken:       "atk",
		ExpiresAt:         now,
		ClientID:          "cid",
		Kid:               "kid",
		TokenEndpoint:     "https://example/token",
		ActivationVersion: "av",
		KeyThumbprint:     "thumb",
		NegUntil:          now.Add(time.Minute),
		NegReason:         "409 workload_identity_unavailable",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.ExpiresAt.Equal(e.ExpiresAt) || !got.NegUntil.Equal(e.NegUntil) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, e)
	}
}
