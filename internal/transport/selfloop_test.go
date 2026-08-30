package transport

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Selfloop_test.go; the self-loop fix, measured by its effect rather than its
// mechanism.

// selfLoopProbeEnv makes the re-executed test binary run the probe instead of
// the suite. Its value selects which half of the drill runs.
const selfLoopProbeEnv = "OPENBOX_TRANSPORT_SELFLOOP_PROBE"

// TestMain intercepts the re-exec.
func TestMain(m *testing.M) {
	switch os.Getenv(selfLoopProbeEnv) {
	case "cleared":
		os.Exit(runSelfLoopProbe(true))
	case "not-cleared":
		os.Exit(runSelfLoopProbe(false))
	}
	os.Exit(m.Run())
}

// runSelfLoopProbe answers one question in a fresh process: with the proxy
// environment set, does `http.ProxyFromEnvironment` route a provider request
// through this relay's own listen address?
func runSelfLoopProbe(clear bool) int {
	if clear {
		clearInheritedProxyEnv()
	}

	req, err := http.NewRequest(http.MethodPost, "https://"+DefaultInterceptHost+"/v1/messages", nil)
	if err != nil {
		os.Stderr.WriteString("probe: build request: " + err.Error() + "\n")
		return 2
	}
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
func TestTheUpstreamLegWouldNotDialThisRelay(t *testing.T) {
	selfAddr := "http://" + DefaultAddr

	t.Run("with the fix, no proxy resolves", func(t *testing.T) {
		out, code := runProbe(t, "cleared", selfAddr)
		if code != 0 {
			t.Errorf("the upstream leg would still route through %s — the relay would dial itself "+
				"and recurse until sockets run out. probe exit=%d output=%q", selfAddr, code, out)
		}
	})

	// Same process shape, same environment, clearing removed; and it must resolve
	// the proxy.
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

// TestTheClearedKeysAreTheOnesNetHTTPReads. Clearing a key it does not read
// would be harmless; failing to clear one it does read is the whole defect.
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

func runProbe(t *testing.T, mode, proxyURL string) (string, int) {
	t.Helper()
	if _, err := url.Parse(proxyURL); err != nil {
		t.Fatalf("bad probe proxy URL %q: %v", proxyURL, err)
	}

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
