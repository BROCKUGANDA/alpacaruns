// ui.go — embeds the Next.js static export (`dashboard/out`) into
// the Go binary and serves it on the same :8080 port as the JSON
// API. Two goals:
//
//   1. One binary, one port, one process: a single `alpacaruns serve`
//      deploys the full demo (live, trades, brain, controls) plus
//      the Go API. No separate Node container needed for the demo.
//   2. The JSON API still wins on `/api/*` — UI assets are served
//      last so we never shadow an API endpoint.
//
// Build prerequisite: `cd dashboard && npm install && npm run build`
// produces ./dashboard/out which is what this file embeds.
//
// The embed is intentionally a build-time decision: missing assets
// (unbuilt dashboard) trip a panic at compile time so the deploy
// fails loudly rather than serving 404s in production.

package api

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:ui
var uiFS embed.FS

// uiSubFS returns the embedded dashboard rooted at "ui/".
func uiSubFS() (fs.FS, error) {
	return fs.Sub(uiFS, "ui")
}

// uiHandler serves the embedded dashboard. The static export emits
// directories with index.html inside (welcome/index.html etc.) so
// we let http.FileServer do its native directory → index.html rewrite
// and only special-case the bare "/" path. Asset caching:
//
//   - HTML pages: no-store (so deploys always surface the latest UI).
//   - Content-hashed /_next/static/* chunks: long max-age (immutable).
//   - Everything else: a short cache so the favicon/logo don't get
//     hammered but also get refreshed after a redeploy.
func uiHandler() (http.Handler, error) {
	root, err := uiSubFS()
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bare "/" → /welcome so the splash is the first impression.
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/welcome", http.StatusTemporaryRedirect)
			return
		}
		setAssetCacheHeaders(w, r.URL.Path)
		fileServer.ServeHTTP(w, r)
	}), nil
}

func setAssetCacheHeaders(w http.ResponseWriter, p string) {
	ext := path.Ext(p)
	switch {
	case ext == ".html" || ext == "":
		w.Header().Set("Cache-Control", "no-store")
	case strings.HasPrefix(strings.TrimPrefix(p, "/"), "_next/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case ext == ".ico" || ext == ".png" || ext == ".svg" || ext == ".webmanifest":
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
}

// uiMounted is exposed for tests / health checks.
func uiMounted() bool {
	root, err := uiSubFS()
	if err != nil {
		return false
	}
	_, err = fs.Stat(root, "index.html")
	return err == nil || errors.Is(err, fs.ErrExist)
}