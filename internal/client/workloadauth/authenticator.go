package workloadauth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SDKVersion identifies this client over X-OpenBox-SDK-Version and
// User-Agent, in the same "openbox-{engine}-{language}-v{version}" family the
// Python SDK uses.
const SDKVersion = "openbox-shift-left-go-v1"

// Authenticator is the single-flight workload-identity client: it owns the
// private key, the API key, the cache and the HTTP client, and produces a
// bearer token cold (bootstrap, assert, exchange) or warm (cache hit).
type Authenticator struct {
	apiKey     string
	key        *rsa.PrivateKey
	thumb      string
	http       *http.Client
	baseURL    string
	sdkVersion string
	cache      Cache

	// now is time.Now by default; tests override it to control cache
	// freshness and the 60s negative-cache window without sleeping.
	now func() time.Time

	mu sync.Mutex
}

// NewAuthenticator builds an Authenticator from a private key in either form
// ParsePrivateKey accepts. An empty cachePath selects the in-memory cache
// (lane daemons, git-action); any other path selects a FileCache at that
// path. hc defaults to http.DefaultClient when nil.
func NewAuthenticator(apiKey, privateKey, cachePath, baseURL string, hc *http.Client) (*Authenticator, error) {
	key, err := ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	thumb, err := Thumbprint(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("workloadauth: computing key thumbprint: %w", err)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	var cache Cache
	if cachePath == "" {
		cache = NewMemoryCache()
	} else {
		cache = &FileCache{Path: cachePath}
	}
	return &Authenticator{
		apiKey:     apiKey,
		key:        key,
		thumb:      thumb,
		http:       hc,
		baseURL:    baseURL,
		sdkVersion: SDKVersion,
		cache:      cache,
		now:        time.Now,
	}, nil
}

// Token returns a bearer token: from cache when fresh (fromCache=true), or
// cold via bootstrap, assertion and exchange (fromCache=false). Concurrent
// callers single-flight through mu with a double check, so N cold callers
// produce exactly one bootstrap and one exchange. A live negative-cache
// entry (a bootstrap 401/403/404/409 within the last 60s) returns its
// remembered error without any network call.
func (a *Authenticator) Token(ctx context.Context) (token string, fromCache bool, err error) {
	if tok, ok, cerr := a.checkCache(); ok {
		return tok, true, nil
	} else if cerr != nil {
		return "", false, cerr
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if tok, ok, cerr := a.checkCache(); ok {
		return tok, true, nil
	} else if cerr != nil {
		return "", false, cerr
	}

	doc, berr := Bootstrap(ctx, a.http, a.baseURL, a.apiKey, a.sdkVersion)
	if berr != nil {
		var werr *Error
		if errors.As(berr, &werr) && isNegativeCacheStatus(werr.Status) {
			now := a.now()
			_ = a.cache.Store(Entry{
				NegUntil:  now.Add(negativeCacheTTL),
				NegReason: werr.Reason,
			})
		}
		return "", false, berr
	}

	assertion, aerr := BuildAssertion(a.key, doc)
	if aerr != nil {
		return "", false, aerr
	}

	result, eerr := Exchange(ctx, a.http, doc, assertion)
	if eerr != nil {
		return "", false, eerr
	}

	now := a.now()
	entry := Entry{
		AccessToken:       result.AccessToken,
		ExpiresAt:         now.Add(cappedLifetime(result.ExpiresIn)),
		ClientID:          doc.ClientID,
		Kid:               doc.Kid,
		TokenEndpoint:     doc.TokenEndpoint,
		ActivationVersion: doc.ActivationVersion,
		KeyThumbprint:     a.thumb,
	}
	if serr := a.cache.Store(entry); serr != nil {
		return "", false, fmt.Errorf("workloadauth: storing token cache: %w", serr)
	}

	return result.AccessToken, false, nil
}

// Invalidate clears this Authenticator's cache (the file, for a FileCache;
// the in-process entry, for a memory cache), so the next Token call goes
// cold.
func (a *Authenticator) Invalidate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cache.Clear()
}

// checkCache reports a fresh cached token (ok=true, err=nil), a live
// negative-cache error (ok=false, err!=nil), or neither (ok=false, err=nil)
// meaning the caller must go cold.
func (a *Authenticator) checkCache() (string, bool, error) {
	entry, has := a.cache.Load()
	if !has {
		return "", false, nil
	}
	now := a.now()
	if entry.fresh(now, a.thumb) {
		return entry.AccessToken, true, nil
	}
	if entry.negative(now) {
		return "", false, &Error{Stage: StageBootstrap, Reason: entry.NegReason}
	}
	return "", false, nil
}

// isNegativeCacheStatus reports whether a bootstrap failure at this status
// is remembered for negativeCacheTTL, per the four codes the spec pins:
// 401/403/404/409.
func isNegativeCacheStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return true
	default:
		return false
	}
}

// cappedLifetime bounds a server's expires_in at maxCacheLifetime.
func cappedLifetime(expiresInSeconds float64) time.Duration {
	if expiresInSeconds > maxCacheLifetime.Seconds() {
		expiresInSeconds = maxCacheLifetime.Seconds()
	}
	return time.Duration(expiresInSeconds * float64(time.Second))
}
