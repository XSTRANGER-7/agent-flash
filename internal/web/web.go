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
		if strings.HasPrefix(p, "/v1/") { // unknown API path, never the SPA
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
			return
		}
		if p != "/" && strings.HasSuffix(p, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("X-Frame-Options", "DENY")
		switch {
		case p == "/" || p == "/index.html" || p == "/sw.js" || p == "/manifest.webmanifest" ||
			strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css"):
			// The shell is not fingerprinted: browsers must check for a new copy
			// every time, or a fix can sit unseen in their cache for an hour.
			h.Set("Cache-Control", "no-cache")
		case strings.HasPrefix(p, "/fonts/"):
			h.Set("Cache-Control", "public, max-age=31536000, immutable") // a font file never changes in place
		default:
			h.Set("Cache-Control", "public, max-age=3600")
		}
		switch path.Ext(p) {
		case ".webmanifest":
			h.Set("Content-Type", "application/manifest+json")
		case ".woff2":
			h.Set("Content-Type", "font/woff2")
		}
		files.ServeHTTP(w, r)
	})
}
