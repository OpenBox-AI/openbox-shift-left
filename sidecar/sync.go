package sidecar

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// isNotExist reports a "file not found" through a wrapped (%w) error chain —
// os.IsNotExist does not unwrap, errors.Is does.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// defaultSyncInterval is how often the out-of-band loop refreshes the local
// bundle. It is deliberately coarse: the bundle is policy, which changes rarely,
// and sync is OFF the hot path (INV-3b) — the decision never waits on it.
const defaultSyncInterval = 60 * time.Second

// BundleSource fetches the current policy bundle from OUT-OF-BAND. It is the
// pluggable seam for where policy comes from:
//   - FileBundleSource — a local bundle file (Phase-1 default; an operator or a
//     management agent drops the bundle there).
//   - [EXT-opa-bundle] a signed core/management-plane fetch — the external
//     follow-up (cross-repo recon 2026-07-13: core distributes NO bundle today;
//     its rego lives in Postgres). It would reuse the client signer (SL-11's
//     signed GET pattern) and MUST stay off the decision path.
//
// Fetch returns (nil, "", nil) to mean "no change / nothing to load" without it
// being an error (so an absent local file at cold start is not logged as a
// failure every interval).
type BundleSource interface {
	Fetch(ctx context.Context) (*Bundle, error)
}

// syncLoop primes the server's bundle from src once, then (only when interval>0)
// keeps re-loading it from the LOCAL file on that interval. A fetch/parse failure
// is logged and the CURRENT bundle is KEPT — a bad reload never clears policy and
// never blocks an in-flight decision (the SetBundle swap is atomic).
//
// STORY-E6-S8 / ADR-0005 §Decision-3 — the daemon does ZERO network I/O and the
// 60 s background poll is RETIRED as the freshness mechanism. Freshness is now a
// CLIENT-SIDE session-start staleness check (adapter) that pulls the org policy
// and re-runs `dev sync`; the daemon merely loads whatever `dev sync` wrote via
// the local FileBundleSource. So the default (interval<=0) is PRIME-ONCE with NO
// background ticker. A positive --sync-interval is still accepted (back-compat):
// it re-polls the LOCAL bundle file — never the network — for an operator who
// wants the running daemon to pick up an out-of-band bundle edit mid-session; the
// mtime gate keeps an unchanged file from being re-loaded needlessly.
func syncLoop(ctx context.Context, s *Server, src BundleSource, interval time.Duration, log client.Logger) {
	if log == nil {
		log = nopLogger{}
	}
	// Prime once immediately so a freshly started daemon loads the local policy
	// without waiting (still off the hot path — the server serves fail-open until
	// this returns).
	syncOnce(ctx, s, src, log)

	if interval <= 0 {
		return // prime-once: no background loop, no re-poll (the E6-S8 default)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			syncOnce(ctx, s, src, log)
		}
	}
}

func syncOnce(ctx context.Context, s *Server, src BundleSource, log client.Logger) {
	if log == nil {
		log = nopLogger{}
	}
	b, err := src.Fetch(ctx)
	if err != nil {
		// Keep the current bundle; a transient sync failure must not disable or
		// change enforcement. Non-secret diagnostic only (INV-1).
		log.Printf("openbox sidecar: bundle sync failed (keeping current policy): %v", err)
		return
	}
	if b == nil {
		return // no change / nothing to load
	}
	s.SetBundle(b)
	log.Printf("openbox sidecar: loaded policy bundle version=%q rules=%d", b.Version, len(b.Rules))
}

// FileBundleSource loads the bundle from a local file. It is the Phase-1 default
// source: an operator (or a management agent) writes/updates the bundle file and
// the daemon picks it up on the next sync tick.
//
// It returns (nil, nil) — "no change" — when the file is absent (cold start:
// serve fail-open, no error) OR when the file's mtime is unchanged since the last
// successful load. Reporting "no change" (rather than re-loading identical
// content every tick) keeps SetBundle — and therefore loadedAt — from being
// reset on unchanged policy, so the Stale flag actually reflects policy age and
// the sync log stays quiet. Use NewFileBundleSource so the mtime state is shared.
type FileBundleSource struct {
	Path string

	mu      sync.Mutex
	lastMod time.Time
	loaded  bool
}

// NewFileBundleSource builds a file-backed source for path.
func NewFileBundleSource(path string) *FileBundleSource { return &FileBundleSource{Path: path} }

// Fetch implements BundleSource. The pointer receiver holds the last-loaded mtime
// so an unchanged file is a genuine no-op.
func (f *FileBundleSource) Fetch(_ context.Context) (*Bundle, error) {
	fi, err := os.Stat(f.Path)
	if err != nil {
		if isNotExist(err) {
			// Absent file is not a failure — the operator has not provisioned policy
			// yet; the daemon stays fail-open until they do.
			return nil, nil
		}
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded && !fi.ModTime().After(f.lastMod) {
		return nil, nil // unchanged since last successful load
	}
	b, err := LoadBundleFile(f.Path)
	if err != nil {
		// Do NOT advance lastMod: a malformed edit is retried next tick, and the
		// current policy is kept (syncOnce keeps current on error).
		return nil, err
	}
	f.lastMod = fi.ModTime()
	f.loaded = true
	return b, nil
}
