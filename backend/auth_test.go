package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fakeIDToken(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestParseAndCheckClaims(t *testing.T) {
	c, err := parseIDToken(fakeIDToken(map[string]any{
		"aud": "cid", "tid": "t1", "name": "Ann", "preferred_username": "ann@example.com",
		"groups": []string{"g0", "g1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Ann" || c.Email != "ann@example.com" {
		t.Fatalf("claims %+v", c)
	}
	if err := checkClaims(c, "cid", "t1", "g1"); err != nil {
		t.Fatalf("member rejected: %v", err)
	}
	if err := checkClaims(c, "cid", "t1", "g9"); err == nil {
		t.Fatal("non-member accepted")
	}
	if err := checkClaims(c, "other", "t1", "g1"); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if err := checkClaims(c, "cid", "t2", "g1"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
	if _, err := parseIDToken("nope"); err == nil {
		t.Fatal("malformed token accepted")
	}
}

func TestHandleLogoutDropsSession(t *testing.T) {
	sessions.Lock()
	sessions.m["sid1"] = &session{Name: "Ann", Email: "ann@example.com", dbs: map[string]*sql.DB{}}
	sessions.Unlock()

	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: "sid1"})
	w := httptest.NewRecorder()
	handleLogout(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	sessions.Lock()
	_, ok := sessions.m["sid1"]
	sessions.Unlock()
	if ok {
		t.Fatal("session still present after logout")
	}

	r2 := httptest.NewRequest("GET", "/api/me", nil)
	r2.AddCookie(&http.Cookie{Name: "sid", Value: "sid1"})
	w2 := httptest.NewRecorder()
	withSession(func(http.ResponseWriter, *http.Request, *session) {
		t.Fatal("handler called for unknown sid")
	})(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w2.Code, http.StatusUnauthorized)
	}
}

func TestDevModeSession(t *testing.T) {
	devSession = &session{Name: "dev", Email: "dev", dbs: map[string]*sql.DB{}}
	defer func() { devSession = nil }()
	var got string
	h := withSession(func(w http.ResponseWriter, r *http.Request, s *session) { got = s.Name })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/me", nil))
	if got != "dev" || rec.Code != http.StatusOK {
		t.Fatalf("name %q code %d", got, rec.Code)
	}
}

func TestNoSessionIs401(t *testing.T) {
	h := withSession(func(w http.ResponseWriter, r *http.Request, s *session) { t.Fatal("handler ran") })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestExpiredSessionIs401(t *testing.T) {
	sessions.Lock()
	sessions.m["old"] = &session{expires: time.Now().Add(-time.Second), dbs: map[string]*sql.DB{}}
	sessions.m["fresh"] = &session{expires: time.Now().Add(time.Hour), dbs: map[string]*sql.DB{}}
	sessions.Unlock()
	defer func() { sessions.Lock(); delete(sessions.m, "fresh"); sessions.Unlock() }()
	h := withSession(func(w http.ResponseWriter, r *http.Request, s *session) { w.WriteHeader(http.StatusOK) })
	for sid, want := range map[string]int{"old": http.StatusUnauthorized, "fresh": http.StatusOK} {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code != want {
			t.Fatalf("%s: code %d, want %d", sid, rec.Code, want)
		}
	}
	sessions.Lock()
	_, ok := sessions.m["old"]
	sessions.Unlock()
	if ok {
		t.Fatal("expired session not dropped")
	}
}
