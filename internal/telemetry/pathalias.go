package telemetry

import (
	"context"
	"maps"
	"net/http"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configmiddleware"
)

// Muse exports to <endpoint>/muse-code/telemetry/{logs,traces}: its own path
// prefix, not the OTLP default, and not configurable on its side. The
// collector's OTLP receiver serves exactly one path per signal, so a second
// spelling is an HTTP middleware that rewrites the request path before the
// collector's mux sees it. One server, one listener, one set of handlers: a
// Muse export is decoded by the same code as every other.
const (
	// MuseLogsPath and MuseTracesPath are where Muse posts each signal.
	MuseLogsPath   = "/muse-code/telemetry/logs"
	MuseTracesPath = "/muse-code/telemetry/traces"
)

// pathAliases maps an alternate path to the collector's own. Only these two:
// Muse exports no metrics, and an unlisted path must still 404 rather than be
// quietly accepted.
var pathAliases = map[string]string{
	MuseLogsPath:   "/v1/logs",
	MuseTracesPath: "/v1/traces",
}

// aliasExtensionID names the middleware in the host's extension map. It is an
// identifier between this file's two halves and nothing else.
var aliasExtensionID = component.NewID(component.MustNewType("openboxpathalias"))

// pathAlias is an HTTP server middleware in the collector's own extension
// shape (its GetHTTPHandler matches extensionmiddleware.HTTPServer, which the
// server looks up by interface): it needs no lifecycle, so Start and Shutdown
// do nothing.
type pathAlias struct{}

func (pathAlias) Start(context.Context, component.Host) error { return nil }
func (pathAlias) Shutdown(context.Context) error              { return nil }

func (pathAlias) GetHTTPHandler(context.Context) (func(context.Context, http.Handler) (http.Handler, error), error) {
	return func(_ context.Context, next http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if to, ok := pathAliases[r.URL.Path]; ok {
				// A copy: the caller's request, and its URL, stay as received.
				u := *r.URL
				u.Path, u.RawPath = to, ""
				r = r.WithContext(r.Context())
				r.URL = &u
			}
			next.ServeHTTP(w, r)
		}), nil
	}, nil
}

// aliasHost adds the alias middleware to whatever extensions the host has.
type aliasHost struct{ component.Host }

func (h aliasHost) GetExtensions() map[component.ID]component.Component {
	ext := map[component.ID]component.Component{}
	if h.Host != nil {
		maps.Copy(ext, h.Host.GetExtensions())
	}
	ext[aliasExtensionID] = pathAlias{}
	return ext
}

// aliasMiddleware is the server config entry that selects the middleware.
var aliasMiddleware = []configmiddleware.Config{{ID: aliasExtensionID}}
