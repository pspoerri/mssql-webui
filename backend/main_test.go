package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPAHandlerFallsBackToIndex(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":    {Data: []byte("<html>spa</html>")},
		"assets/app.js": {Data: []byte("js")},
	}
	h := spaHandler(dist)
	cases := []struct {
		path string
		code int
		body string
	}{
		{"/", 200, "<html>spa</html>"},
		{"/help", 200, "<html>spa</html>"},
		{"/s/srv/d/db/t/dbo/orders", 200, "<html>spa</html>"},
		{"/s/srv/d/db/console", 200, "<html>spa</html>"},
		{"/assets/app.js", 200, "js"},
		{"/api/nope", 404, ""},
		{"/auth/nope", 404, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.code || (c.body != "" && rec.Body.String() != c.body) {
			t.Errorf("%s: got %d %q, want %d %q", c.path, rec.Code, rec.Body.String(), c.code, c.body)
		}
	}
}

func TestProtect(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for _, c := range []struct {
		name, method, host, site string
		dev                      bool
		want                     int
	}{
		{"same-origin POST", "POST", "sql.example", "same-origin", false, 200},
		{"cross-site POST", "POST", "sql.example", "cross-site", false, 403},
		{"same-site sibling POST", "POST", "sql.example", "same-site", false, 403},
		{"cross-site GET", "GET", "sql.example", "cross-site", false, 200},
		{"no browser headers (curl)", "POST", "sql.example", "", false, 200},
		{"dev: localhost", "GET", "localhost:8080", "", true, 200},
		{"dev: 127.0.0.1", "POST", "127.0.0.1:8080", "same-origin", true, 200},
		{"dev: [::1]", "GET", "[::1]:8080", "", true, 200},
		{"dev: rebound name", "GET", "evil.example:8080", "", true, 403},
		{"dev: cross-site POST", "POST", "localhost:8080", "cross-site", true, 403},
	} {
		r := httptest.NewRequest(c.method, "/api/s/x/d/y/query", nil)
		r.Host = c.host
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		protect(ok, c.dev).ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
