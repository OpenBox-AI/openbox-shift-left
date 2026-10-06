package client

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestTokenObserverSeesEveryAcquisitionButNeverTheToken: the local trace
// learns of each workload-token acquisition -- cached or fetched, how long,
// whether it failed -- through SetTokenObserver, and the observer is never
// handed the bearer itself.
func TestTokenObserverSeesEveryAcquisitionButNeverTheToken(t *testing.T) {
	var mu sync.Mutex
	var seen []TokenAcquisition
	restore := SetTokenObserver(func(a TokenAcquisition) {
		mu.Lock()
		seen = append(seen, a)
		mu.Unlock()
	})
	defer restore()

	srv, _, _ := coreMirrorServer(t, func(int) (int, string) { return 200, "ALLOW" })
	c, _ := newTestClient(t, srv.URL, false)
	c.tokens = &fakeTokenSource{token: testWorkloadToken, fromCache: true}
	if _, err := c.Emit(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	c.tokens = &fakeTokenSource{err: errors.New("exchange refused")}
	_, _ = c.Emit(context.Background(), sampleEvent())

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("observer saw %d acquisitions, want at least 2", len(seen))
	}
	if !seen[0].FromCache || seen[0].Err != nil {
		t.Errorf("first acquisition = %+v, want a cache hit with no error", seen[0])
	}
	if last := seen[len(seen)-1]; last.Err == nil {
		t.Errorf("last acquisition = %+v, want the exchange failure", last)
	}
	for _, a := range seen {
		if strings.Contains(a.String(), testWorkloadToken) {
			t.Fatalf("an acquisition record carries the bearer: %s", a)
		}
	}
}
