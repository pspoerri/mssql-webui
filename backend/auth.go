package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"
)

var (
	oauthCfg      *oauth2.Config
	tenantID      string
	allowedGroup  string
	secureCookies bool
)

// devSession is set when DEV_USER is given: Entra is skipped, every request
// runs as this one session, and servers connect with the SQL login in their URL.
// ponytail: one shared identity for anyone who can reach the port. Never set
// DEV_USER in production.
var devSession *session

func initAuth() {
	if u := os.Getenv("DEV_USER"); u != "" {
		if os.Getenv("CLIENT_SECRET") != "" || os.Getenv("TENANT_ID") != "" {
			log.Fatal("DEV_USER cannot be combined with Entra configuration (TENANT_ID/CLIENT_SECRET); choose one")
		}
		log.Printf("DEV_USER=%s: Entra login disabled, using SQL logins from SQL_SERVERS", u)
		devSession = &session{Name: u, Email: u, dbs: map[string]*sql.DB{}}
		return
	}
	tenantID = mustEnv("TENANT_ID")
	allowedGroup = mustEnv("ALLOWED_GROUP_ID")
	oauthCfg = &oauth2.Config{
		ClientID:     mustEnv("CLIENT_ID"),
		ClientSecret: mustEnv("CLIENT_SECRET"),
		RedirectURL:  mustEnv("REDIRECT_URL"),
		Endpoint:     microsoft.AzureADEndpoint(tenantID),
		Scopes:       []string{"openid", "profile", "offline_access", "https://database.windows.net/user_impersonation"},
	}
	// Plain http redirect URL means local dev; browsers drop Secure cookies there.
	secureCookies = strings.HasPrefix(oauthCfg.RedirectURL, "https://")
}

// sessionTTL caps how long a login, and therefore a leaked sid cookie, stays
// usable. The refresh token would otherwise keep minting SQL tokens for up
// to 90 days. Re-login also re-checks group membership at Entra.
const sessionTTL = 12 * time.Hour

type session struct {
	Name    string
	Email   string
	expires time.Time
	ts      oauth2.TokenSource
	mu      sync.Mutex
	dbs     map[string]*sql.DB // "server/database" -> pool, filled by sql.go
}

// ponytail: in-memory sessions, single instance, live until logout, sessionTTL
// or restart. Expired sessions are dropped on their next request; add a
// sweeper if abandoned logins pile up. Swap the map for Redis if scaled out.
var sessions = struct {
	sync.Mutex
	m map[string]*session
}{m: map[string]*session{}}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type claims struct {
	Aud    string   `json:"aud"`
	Tid    string   `json:"tid"`
	Name   string   `json:"name"`
	Email  string   `json:"preferred_username"`
	Groups []string `json:"groups"`
}

// parseIDToken reads claims without verifying the signature. The token comes
// straight from the token endpoint over TLS, which OIDC Core 3.1.3.7 accepts
// in place of a signature check for the code flow.
func parseIDToken(tok string) (claims, error) {
	var c claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, errors.New("malformed id_token")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// dropSession removes id from sessions.m and closes any DB pools it held.
func dropSession(id string) {
	sessions.Lock()
	s := sessions.m[id]
	delete(sessions.m, id)
	sessions.Unlock()
	if s != nil {
		s.mu.Lock()
		for _, db := range s.dbs {
			db.Close()
		}
		s.mu.Unlock()
	}
}

func checkClaims(c claims, clientID, tenant, group string) error {
	if c.Aud != clientID {
		return errors.New("id_token audience mismatch")
	}
	if c.Tid != tenant {
		return errors.New("id_token tenant mismatch")
	}
	for _, g := range c.Groups {
		if g == group {
			return nil
		}
	}
	return errors.New("not a member of the allowed group")
}

func registerAuth(mux *http.ServeMux) {
	if devSession == nil {
		mux.HandleFunc("GET /auth/login", handleLogin)
		mux.HandleFunc("GET /auth/callback", handleCallback)
	}
	mux.HandleFunc("POST /auth/logout", handleLogout)
	mux.HandleFunc("GET /api/me", withSession(func(w http.ResponseWriter, r *http.Request, s *session) {
		writeJSON(w, http.StatusOK, map[string]string{"name": s.Name, "email": s.Email, "version": version})
	}))
}

func setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: secureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomID()
	setCookie(w, "oauth_state", state, "/auth", 300)
	http.Redirect(w, r, oauthCfg.AuthCodeURL(state, oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie("oauth_state")
	if err != nil || c.Value == "" || c.Value != q.Get("state") {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		http.Error(w, e+": "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	tok, err := oauthCfg.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	cl, err := parseIDToken(raw)
	if err != nil {
		log.Printf("id_token: %v", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	if err := checkClaims(cl, oauthCfg.ClientID, tenantID, allowedGroup); err != nil {
		http.Error(w, "access denied: "+err.Error(), http.StatusForbidden)
		return
	}
	if old, err := r.Cookie("sid"); err == nil {
		dropSession(old.Value)
	}
	id := randomID()
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	sessions.Lock()
	sessions.m[id] = &session{
		Name:    cl.Name,
		Email:   cl.Email,
		expires: time.Now().Add(sessionTTL),
		ts:      oauthCfg.TokenSource(ctx, tok),
		dbs:     map[string]*sql.DB{},
	}
	sessions.Unlock()
	setCookie(w, "sid", id, "/", 0)
	setCookie(w, "oauth_state", "", "/auth", -1)
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		dropSession(c.Value)
	}
	setCookie(w, "sid", "", "/", -1)
	w.WriteHeader(http.StatusNoContent)
}

func withSession(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if devSession != nil {
			h(w, r, devSession)
			return
		}
		c, err := r.Cookie("sid")
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
			return
		}
		sessions.Lock()
		s := sessions.m[c.Value]
		sessions.Unlock()
		if s != nil && time.Now().After(s.expires) {
			dropSession(c.Value)
			s = nil
		}
		if s == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
			return
		}
		h(w, r, s)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
