package sidecar

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Config configures the resident daemon `openbox sidecar serve` runs.
type Config struct {
	// SocketPath is where the daemon binds. Empty → DefaultSocketPath().
	SocketPath string

	// BundlePath is the local policy file the default FileBundleSource loads.
	// Empty → DefaultBundlePath(). Absent file → serve fail-open until provisioned.
	BundlePath string

	// Source overrides where policy comes from. Nil → FileBundleSource{BundlePath}.
	// (The signed core/management fetch — [EXT-opa-bundle] — plugs in here.)
	Source BundleSource

	// SyncInterval is the LOCAL bundle re-poll period (back-compat only). Zero
	// (default) → PRIME-ONCE: the daemon loads the local bundle at startup and does
	// NO background re-poll and NO network I/O (STORY-E6-S8 / ADR-0005 §Decision-3 —
	// freshness is the client-side session-start staleness check, not a daemon
	// poll). A positive value re-loads the LOCAL file on that interval (never the
	// network) for an operator who wants mid-session bundle edits picked up.
	SyncInterval time.Duration

	// Freshness marks a bundle Stale past this age. Zero → 5m.
	Freshness time.Duration

	// Logger receives non-secret lifecycle/diagnostic lines (INV-1). Nil discards.
	Logger client.Logger
}

// DefaultBundlePath is where the daemon looks for its local policy bundle when
// BundlePath is unset: alongside the socket, per-user.
func DefaultBundlePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir + "/openbox/policy-bundle.json"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home + "/.config/openbox/policy-bundle.json"
	}
	return "policy-bundle.json"
}

// Serve runs the decision daemon until ctx is cancelled (the cli wires ctx to
// SIGINT/SIGTERM), then shuts down gracefully: stop accepting, drain in-flight
// handlers, remove the socket. It is the body of `openbox sidecar serve`.
//
// Startup order matters for the fail-open contract: the socket is bound and
// serving (answering fail-open, no bundle yet) BEFORE the first sync completes,
// so a hook that dials during startup gets a prompt allow rather than a refused
// connection. The bundle then loads out-of-band.
func Serve(ctx context.Context, cfg Config) error {
	log := cfg.Logger
	if log == nil {
		log = nopLogger{}
	}
	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}

	if err := prepareSocketDir(socketPath); err != nil {
		return err
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("bind sidecar socket %s: %w", socketPath, err)
	}
	// Lock the socket to the owning user (INV-1). Best-effort chmod after bind —
	// the umask may already have restricted it, but be explicit.
	if err := os.Chmod(socketPath, socketFilePerm); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod sidecar socket %s: %w", socketPath, err)
	}
	// The unix listener removes the socket file on Close(); remove it explicitly
	// too in case of an abnormal exit path.
	defer os.Remove(socketPath)

	srv := NewServer(ServerConfig{
		Freshness: cfg.Freshness,
		Logger:    log,
	})

	// Out-of-band policy sync (off the hot path).
	src := cfg.Source
	if src == nil {
		bp := cfg.BundlePath
		if bp == "" {
			bp = DefaultBundlePath()
		}
		src = NewFileBundleSource(bp)
	}
	go syncLoop(ctx, srv, src, cfg.SyncInterval, log)

	log.Printf("openbox sidecar: serving decisions on %s (fail-open until policy loads)", socketPath)
	return srv.Serve(ctx, ln)
}
