// branding.go — embed the dashboard's brand assets (logo + favicons)
// into the Go binary so /logo.svg, /favicon.svg, /favicon.png, /favicon.ico
// are always reachable, even when the Next.js dashboard is deployed
// separately or not yet built. Source-of-truth lives in
// dashboard/public/ — files here are copied at release time and must
// stay in sync.
//
// The Go API server never renders the dashboard itself; it just
// serves these static files so curling /logo.svg works against a
// bare `alpacaruns serve` deployment. The actual dashboard UI is
// served by Next.js on a different origin and proxies API calls
// through NEXT_PUBLIC_API_URL.
package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed branding/*
var brandingFS embed.FS

// brandingSub returns the branding sub-filesystem as fs.FS so it can
// be passed to http.ServeFileFS (which requires fs.FS, not http.FileSystem).
func brandingSub() fs.FS {
	sub, err := fs.Sub(brandingFS, "branding")
	if err != nil {
		panic(err) // embed cannot fail at runtime
	}
	return sub
}

// brandingAlias returns a tiny handler that serves one file from the
// embedded branding sub-FS. Used by the well-known paths (/logo.svg,
// /favicon.*) so a bare curl against the API host returns the brand
// even when the Next.js dashboard is deployed elsewhere.
func brandingAlias(name string) http.HandlerFunc {
	sub := brandingSub()
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, name)
	}
}

// brandingHandler returns an http.Handler that serves the embedded
// branding assets. Convenience aliases at /logo.svg and /favicon.*
// work without a /branding/ prefix so a bare curl against the API
// host returns the brand even when the dashboard app isn't deployed.
func brandingHandler() http.Handler {
	sub := brandingSub()
	fileServer := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()
	mux.Handle("/branding/", http.StripPrefix("/branding/", fileServer))

	// Convenience aliases — every well-known path resolves to one
	// file in the branding sub-FS.
	alias := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, sub, name)
		}
	}
	mux.HandleFunc("/logo.svg", alias("logo.svg"))
	mux.HandleFunc("/favicon.svg", alias("favicon.svg"))
	mux.HandleFunc("/favicon.png", alias("favicon.png"))
	mux.HandleFunc("/favicon.ico", alias("favicon.ico"))
	mux.HandleFunc("/apple-touch-icon.png", alias("favicon.png"))
	return mux
}