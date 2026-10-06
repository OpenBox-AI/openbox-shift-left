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

	// renewTimeout bounds an early renewal (refreshAheadTimeout by default);
	// tests shorten it.
	renewTimeout time.Duration

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

		renewTimeout: refreshAheadTimeout,
	}, nil
}

// Token returns a bearer token: from cache when fresh (fromCache=true), or
// cold via bootstrap, assertion and exchange (fromCache=false). Concurrent
// callers single-flight through mu with a double check, so N cold callers
// produce exactly one bootstrap and one exchange. A live negative-cache
// entry (a bootstrap 401/403/404/409 within the last 60s) returns its
// remembered error without any network call.
//
// A fresh token inside the refreshAhead window is renewed first, bounded by
// refreshAheadTimeout; if that renewal fails transiently the still-usable
// token is returned (fromCache=true) and renewal backs off for
// refreshRetryBackoff across every process sharing the cache.
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

	entry, has := a.cache.Load()
	if !has || !entry.fresh(a.now(), a.thumb) {
		tok, ferr := a.fetch(ctx)
		return tok, false, ferr
	}
	return a.renewEarly(ctx, entry)
}

// Warm renews the cached token when it is inside the refreshAhead window (or
// missing), and does nothing otherwise. The lane daemons call it on a ticker
// against each tool's hook cache file, so a hook process finds a fresh token
// instead of renewing one inside its own evaluation budget.
func (a *Authenticator) Warm(ctx context.Context) error {
	if _, ok, cerr := a.checkCache(); ok || cerr != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, has := a.cache.Load()
	now := a.now()
	if has && entry.negative(now) {
		return nil
	}
	if !has || !entry.fresh(now, a.thumb) {
		_, err := a.fetch(ctx)
		return err
	}
	if !entry.dueForRefresh(now) {
		return nil
	}
	_, _, err := a.renewEarly(ctx, entry)
	return err
}

// renewEarly renews a still-usable entry under a short bound, falling back to
// it on a transient failure. Called with mu held. The returned error is nil
// whenever a usable token is returned; Warm reads a fallback as success too,
// since the next tick retries after the backoff.
func (a *Authenticator) renewEarly(ctx context.Context, entry Entry) (string, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, a.renewTimeout)
	defer cancel()
	tok, err := a.fetch(rctx)
	if err == nil {
		return tok, false, nil
	}
	var werr *Error
	if !errors.As(err, &werr) || !werr.Transient {
		return "", false, err
	}
	entry.RefreshRetryAt = a.now().Add(refreshRetryBackoff)
	_ = a.cache.Store(entry)
	return entry.AccessToken, true, nil
}

// fetch runs the cold path -- bootstrap, assertion, exchange -- and stores
// the result. Called with mu held.
func (a *Authenticator) fetch(ctx context.Context) (string, error) {
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
		return "", berr
	}

	assertion, aerr := BuildAssertion(a.key, doc)
	if aerr != nil {
		return "", aerr
	}

	result, eerr := Exchange(ctx, a.http, doc, assertion)
	if eerr != nil {
		return "", eerr
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
		return "", fmt.Errorf("workloadauth: storing token cache: %w", serr)
	}
	return result.AccessToken, nil
}

// Invalidate clears this Authenticator's cache (the file, for a FileCache;
// the in-process entry, for a memory cache), so the next Token call goes
// cold.
func (a *Authenticator) Invalidate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cache.Clear()
}

// checkCache reports a fresh cached token not yet due for renewal (ok=true,
// err=nil), a live negative-cache error (ok=false, err!=nil), or neither
// (ok=false, err=nil) meaning the caller must renew or go cold.
func (a *Authenticator) checkCache() (string, bool, error) {
	entry, has := a.cache.Load()
	if !has {
		return "", false, nil
	}
	now := a.now()
	if entry.fresh(now, a.thumb) {
		if !entry.dueForRefresh(now) {
			return entry.AccessToken, true, nil
		}
		return "", false, nil
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
