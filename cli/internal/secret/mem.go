package secret

import (
	"sort"
	"strings"
	"sync"
)

// MemStore is an in-memory Store for tests and for the dry-run planner (where no
// real credential is ever written). It is NOT a persistence backend and MUST NOT
// be selected by Detect on any real platform.
type MemStore struct {
	mu   sync.Mutex
	data map[string]string
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{data: map[string]string{}} }

func memKey(service, account string) string { return service + "\x00" + account }

func (m *MemStore) Name() string { return "in-memory (test)" }

func (m *MemStore) Set(service, account, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[memKey(service, account)] = value
	return nil
}

func (m *MemStore) Get(service, account string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[memKey(service, account)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemStore) Delete(service, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, memKey(service, account))
	return nil
}

// Accounts returns the account names stored under service, sorted. Test helper;
// never returns secret values.
func (m *MemStore) Accounts(service string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := service + "\x00"
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(out)
	return out
}

// Len reports how many secrets are stored (test helper).
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}
