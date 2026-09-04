package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed all:web/dist
var webFS embed.FS

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
	mux.HandleFunc("/", spaHandler())
	defaultAddr := ":8080"
	if devSession != nil {
		defaultAddr = "127.0.0.1:8080"
	}
	addr := env("LISTEN_ADDR", defaultAddr)
	log.Printf("listening on %s", addr)
	log.Fatal((&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

// spaHandler serves web/dist and falls back to index.html for unknown paths.
func spaHandler() http.HandlerFunc {
	dist, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatal(err)
	}
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
