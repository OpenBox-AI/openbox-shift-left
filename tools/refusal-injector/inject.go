package main

import (
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
)

// Injector forwards every request to the provider except the ones it is told
// to refuse, which it answers with a candidate shape instead. It is NOT the
// product's relay and must never be confused for one.
type Injector struct {
	// Shape is the candidate injected on a match.
	Shape Shape

	// After is how many matching requests pass through untouched before one is
	// refused.
	After int64

	// Path is the request path suffix that qualifies for injection.
	Path string

	proxy    *httputil.ReverseProxy
	seen     atomic.Int64
	injected atomic.Int64
}

// NewInjector aims the pass-through half at upstream.
func NewInjector(upstream string, shape Shape, after int64, path string) (*Injector, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	inj := &Injector{Shape: shape, After: after, Path: path}
	inj.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = u.Host
		},
	}
	return inj, nil
}

// Matches reports whether this request is the one to refuse.
func (i *Injector) Matches(r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, i.Path) {
		return false
	}
	return i.seen.Add(1) > i.After
}

// Injected is how many responses were fabricated.
func (i *Injector) Injected() int64 { return i.injected.Load() }

// Seen is how many qualifying requests passed through the matcher.
func (i *Injector) Seen() int64 { return i.seen.Load() }

func (i *Injector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !i.Matches(r) {
		i.proxy.ServeHTTP(w, r)
		return
	}
	i.injected.Add(1)
	w.Header().Set("Content-Type", i.Shape.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(i.Shape.Status)
	_, _ = io.WriteString(w, i.Shape.Body)
}
