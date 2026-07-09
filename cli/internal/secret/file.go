package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// --- Opt-in file backend (plaintext-at-rest, 0600) --------------------------
//
// SECURITY POSTURE (SL2-SEC-1): the default backend is the OS keychain and the
// CLI HALTs rather than fall back to plaintext (Detect → ErrNoStore). This file
// backend is the EXPLICIT escape hatch for machines with no OS keyring (headless
// Linux, containers, WSL without a running keyring daemon). It is:
//   - never selected automatically — the operator must ask for it
//     (`--secret-backend file` / OPENBOX_SECRET_BACKEND=file), and the CLI prints
//     a warning when it is used;
//   - written 0600 under the user config dir (0700), NEVER inside a repo, never
//     on an argv, never logged — so INV-1's "not in repo history / not on argv /
//     not in logs" guarantees still hold;
//   - plaintext AT REST, which is the one guarantee it trades away vs. the
//     keychain. That is the conscious, warned tradeoff — hence opt-in only.
//
// On-disk format is a nested JSON object keyed by service then account:
//
//	{ "ai.openbox.dev": { "org/claude-code/api_key": "obx_…", … } }
//
// The adapter hook reads the same format (adapters/claude-code/creds.go) when
// pointed at the file via OPENBOX_SECRET_FILE / dev.json secret_file.

// fileStore is a 0600 JSON-file-backed Store. Concurrency within one process is
// serialized by mu; cross-process writes are made atomic (temp + rename) so a
// concurrent reader never sees a torn file.
type fileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a file-backed Store at path (created lazily on first Set).
func NewFileStore(path string) Store { return &fileStore{path: path} }

// DefaultFilePath is where the file backend lives when no explicit path is
// given. OPENBOX_SECRET_FILE overrides it (tests and the adapter use this to
// agree on one location); otherwise it is <user-config-dir>/openbox/secrets.json.
func DefaultFilePath() string {
	if p := os.Getenv("OPENBOX_SECRET_FILE"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "secrets.json")
}

func (s *fileStore) Name() string { return "plaintext file 0600 (" + s.path + ")" }

// load reads the on-disk map; a missing file is an empty map (not an error).
func (s *fileStore) load() (map[string]map[string]string, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]map[string]string{}, nil
		}
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	m := map[string]map[string]string{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse secret file %s: %w", s.path, err)
	}
	return m, nil
}

// save writes the map atomically with 0600 perms (0700 parent dir).
func (s *fileStore) save(m map[string]map[string]string) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secret dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp secret file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit secret file: %w", err)
	}
	return nil
}

func (s *fileStore) Set(service, account, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if m[service] == nil {
		m[service] = map[string]string{}
	}
	m[service][account] = value
	return s.save(m)
}

func (s *fileStore) Get(service, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return "", err
	}
	if v, ok := m[service][account]; ok {
		return v, nil
	}
	return "", ErrNotFound
}

func (s *fileStore) Delete(service, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if m[service] == nil {
		return nil
	}
	delete(m[service], account)
	if len(m[service]) == 0 {
		delete(m, service)
	}
	return s.save(m)
}

// ErrUnknownBackend is returned by Open for an unrecognized backend name.
var ErrUnknownBackend = errors.New("secret: unknown backend")
