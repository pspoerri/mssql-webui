package main

import (
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
