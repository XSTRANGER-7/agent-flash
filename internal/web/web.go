// Package web embeds the Agent Arena PWA and serves it from the same origin
// as the API, so the browser never needs (or sees) the Flash API key.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var static embed.FS

const csp = "default-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; worker-src 'self'; font-src 'self'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// Handler serves the app shell and assets.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/v1/") {
			http.NotFound(w, r) // unknown API path, never the SPA
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("X-Frame-Options", "DENY")
		switch {
		case p == "/" || p == "/index.html" || p == "/sw.js" || p == "/manifest.webmanifest":
			h.Set("Cache-Control", "no-cache") // SW and shell must update promptly
		default:
			h.Set("Cache-Control", "public, max-age=3600")
		}
		if path.Ext(p) == ".webmanifest" {
			h.Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	})
}
