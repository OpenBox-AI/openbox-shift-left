package transport

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// selfloop_test.go — the self-loop fix, measured by its EFFECT rather than its
// mechanism.
//
// THE DEFECT. `gateway.New` sets `Proxy: http.ProxyFromEnvironment` on its
// upstream client, and `NewIdentityProxy` sets the same on goproxy's transport.
// Activation points a client at this relay by putting
// `HTTPS_PROXY=http://127.0.0.1:8790` in its environment. If the DAEMON inherits
// that variable, this relay's own upstream leg dials the proxy — which is this
// process — and every intercepted call recurses: CONNECT → hijack → serve →
// `Do()` → `HTTPS_PROXY` → CONNECT → … until sockets run out.
//
// WHY THIS FILE EXISTS ALONGSIDE TestNewClearsInheritedProxyEnv. That test
// asserts the MECHANISM: after `New`, the variables are gone from the
// environment. This one asserts the CONSEQUENCE the mechanism is for: that
// `http.ProxyFromEnvironment` — the exact function both legs call — resolves NO
// proxy for a request to the provider. Those are different claims, and only the
// second is the thing that was broken.
//
// WHY A SUBPROCESS. `net/http` resolves the proxy environment ONCE per process
// and caches it behind a `sync.Once` (`envProxyOnce` in net/http/transport.go).
// So the assertion is only meaningful in a process where nothing has consulted
// it yet — and in a test binary, any earlier test that made an HTTP request has
// already poisoned it. Re-exec'ing this same binary with a marker environment
// variable is the only way to get a clean process, and it is also the honest one:
// it reproduces the production ordering, where `New` runs before the daemon makes
// any outbound request at all.

// selfLoopProbeEnv makes the re-executed test binary run the probe instead of the
// suite. Its value selects which half of the drill runs.
const selfLoopProbeEnv = "OPENBOX_TRANSPORT_SELFLOOP_PROBE"

// TestMain intercepts the re-exec. Without a probe marker it runs the suite
// normally.
func TestMain(m *testing.M) {
	switch os.Getenv(selfLoopProbeEnv) {
	case "cleared":
		os.Exit(runSelfLoopProbe(true))
	case "not-cleared":
		os.Exit(runSelfLoopProbe(false))
	}
	os.Exit(m.Run())
}

// runSelfLoopProbe answers one question in a FRESH process: with the proxy
// environment set, does `http.ProxyFromEnvironment` route a provider request
// through this relay's own listen address?
//
// Exit 0 means "no proxy resolved" (the loop cannot happen); exit 1 means a proxy
// WAS resolved (the loop can). Both outcomes are expected — by different halves
// of the drill below.
func runSelfLoopProbe(clear bool) int {
	if clear {
		// The production path. Nothing here dials before this runs, which is the
		// ordering `New` exists to guarantee.
		clearInheritedProxyEnv()
	}

	req, err := http.NewRequest(http.MethodPost, "https://"+DefaultInterceptHost+"/v1/messages", nil)
	if err != nil {
		os.Stderr.WriteString("probe: build request: " + err.Error() + "\n")
		return 2
	}
	// The EXACT function gateway.New and NewIdentityProxy both install as their
	// transport's Proxy. Not a re-implementation of the lookup.
	proxyURL, err := http.ProxyFromEnvironment(req)
	if err != nil {
		os.Stderr.WriteString("probe: ProxyFromEnvironment: " + err.Error() + "\n")
		return 2
	}
	if proxyURL == nil {
		return 0
	}
	os.Stderr.WriteString("probe: resolved proxy " + proxyURL.String() + "\n")
	return 1
}

// TestTheUpstreamLegWouldNotDialThisRelay is the fix, measured.
//
// Both halves run, and the second is what makes the first mean anything: a test
// that only checked "no proxy resolved" would also pass in a world where
// ProxyFromEnvironment never resolves anything, where the environment was never
// set, or where the probe silently failed to run.
func TestTheUpstreamLegWouldNotDialThisRelay(t *testing.T) {
	selfAddr := "http://" + DefaultAddr

	t.Run("with the fix, no proxy resolves", func(t *testing.T) {
		out, code := runProbe(t, "cleared", selfAddr)
		if code != 0 {
			t.Errorf("the upstream leg would still route through %s — the relay would dial itself "+
				"and recurse until sockets run out. probe exit=%d output=%q", selfAddr, code, out)
		}
	})

	// THE NEGATIVE CONTROL. Same process shape, same environment, clearing
	// removed — and it must resolve the proxy. If this half passes, the assertion
	// above is measuring nothing.
	t.Run("without the fix, the relay resolves ITSELF as its proxy", func(t *testing.T) {
		out, code := runProbe(t, "not-cleared", selfAddr)
		if code != 1 {
			t.Errorf("without clearInheritedProxyEnv the probe did NOT resolve a proxy (exit=%d, output=%q). "+
				"Then the positive case above proves nothing: either the environment is not reaching the "+
				"subprocess, or ProxyFromEnvironment no longer reads these variables.", code, out)
		}
		if !strings.Contains(out, DefaultAddr) {
			t.Errorf("the resolved proxy is not this relay's own address %s: %q", DefaultAddr, out)
		}
	})
}

// TestTheClearedKeysAreTheOnesNetHTTPReads.
//
// `http.ProxyFromEnvironment` reads HTTP_PROXY, HTTPS_PROXY and NO_PROXY, each in
// both cases. Clearing a key it does not read would be harmless; FAILING to clear
// one it does read is the whole defect. This pins the two that matter, in both
// spellings, against the real resolver.
//
// NO_PROXY is deliberately NOT cleared and must never be: it is an EXCLUSION
// list, so removing it makes MORE traffic go through a proxy, not less.
func TestTheClearedKeysAreTheOnesNetHTTPReads(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if !clearsKey(key) {
			t.Errorf("%s is read by http.ProxyFromEnvironment but is not in proxyEnvKeys", key)
		}
	}
	if clearsKey("NO_PROXY") || clearsKey("no_proxy") {
		t.Error("NO_PROXY is in proxyEnvKeys. It is an EXCLUSION list — clearing it sends MORE " +
			"traffic through a proxy, which is the opposite of what this guard is for.")
	}
}

func clearsKey(key string) bool {
	for _, k := range proxyEnvKeys {
		if k == key {
			return true
		}
	}
	return false
}

// runProbe re-executes this test binary as a fresh process running one half of
// the drill, and returns its combined output and exit code.
func runProbe(t *testing.T, mode, proxyURL string) (string, int) {
	t.Helper()
	if _, err := url.Parse(proxyURL); err != nil {
		t.Fatalf("bad probe proxy URL %q: %v", proxyURL, err)
	}

	// -run a pattern that matches nothing: TestMain exits before m.Run, but if the
	// marker were ever dropped this keeps the child from re-running the suite (and
	// re-spawning children) instead of hanging or forking without bound.
	cmd := exec.Command(os.Args[0], "-test.run", "XXX_NO_SUCH_TEST")
	cmd.Env = append(os.Environ(),
		selfLoopProbeEnv+"="+mode,
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY="+proxyURL,
		"http_proxy="+proxyURL,
		"https_proxy="+proxyURL,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the probe subprocess: %v (output: %s)", err, out)
	}
	return string(out), code
}
