package web

import (
	"io"
	"net/http"
	"path"
)

// serveStaticFile serves an embedded static/* file with the given content type.
// Used for root-scoped PWA assets (manifest, service worker) that must not live
// only under /static/ (service worker scope).
func serveStaticFile(name, contentType string, headers map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f, err := staticFS.Open(path.Join("static", name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		// Manifest/SW are tiny and version with the binary; allow short cache of icons via /static/.
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(w, f)
	})
}

func registerPWA(mux *http.ServeMux) {
	// Public: install metadata must load before login (and without auth cookies).
	mux.Handle("GET /manifest.webmanifest", serveStaticFile(
		"manifest.webmanifest",
		"application/manifest+json; charset=utf-8",
		map[string]string{"Cache-Control": "public, max-age=3600"},
	))
	// Root-scoped SW so standalone install covers the whole admin UI.
	mux.Handle("GET /sw.js", serveStaticFile(
		"sw.js",
		"application/javascript; charset=utf-8",
		map[string]string{
			"Cache-Control":          "no-cache",
			"Service-Worker-Allowed": "/",
		},
	))
}
