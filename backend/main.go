package main

import (
	"embed"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed all:dist
var webFS embed.FS

// version is set at build time: -ldflags "-X main.version=$(git describe --tags --always --dirty)".
var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing env var %s", key)
	}
	return v
}

func main() {
	initAuth()
	initSQL()
	mux := http.NewServeMux()
	registerAuth(mux)
	registerAPI(mux)
	dist, err := fs.Sub(webFS, "dist")
	if err != nil {
		log.Fatal(err)
	}
	mux.HandleFunc("/", spaHandler(dist))
	defaultAddr := ":8080"
	if devSession != nil {
		defaultAddr = "127.0.0.1:8080"
	}
	addr := env("LISTEN_ADDR", defaultAddr)
	log.Printf("listening on %s; audit log on stdout", addr)
	log.Fatal((&http.Server{Addr: addr, Handler: protect(mux, devSession != nil), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

// protect refuses cross-site requests that change state: a page on another
// origin could otherwise POST SQL to /api/.../query with the user's cookie
// (SameSite=Lax still admits same-site sibling hosts) or, in dev mode, with
// no cookie at all. Dev mode also demands a loopback Host: a DNS-rebinding
// page looks same-origin to the browser, even for GETs, but names its own host.
func protect(h http.Handler, dev bool) http.Handler {
	h = http.NewCrossOriginProtection().Handler(h)
	if !dev {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "dev mode answers only on localhost", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// spaHandler serves the built frontend and falls back to index.html for
// client-side routes such as /help or /s/{srv}/d/{db}/t/{schema}/{table}.
func spaHandler(dist fs.FS) http.HandlerFunc {
	files := http.FileServerFS(dist)
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		if strings.HasPrefix(p, "api/") || strings.HasPrefix(p, "auth/") {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(dist, p); err != nil {
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	}
}
