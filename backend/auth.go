package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	// Optional: empty means any user in the tenant may log in.
	allowedGroup = os.Getenv("ALLOWED_GROUP_ID")
	if allowedGroup == "" {
		log.Print("ALLOWED_GROUP_ID not set: any user in the tenant may log in")
	}
	oauthCfg = &oauth2.Config{
		ClientID:     mustEnv("CLIENT_ID"),
		ClientSecret: mustEnv("CLIENT_SECRET"),
		RedirectURL:  mustEnv("REDIRECT_URL"),
		Endpoint:     microsoft.AzureADEndpoint(tenantID),
		Scopes:       []string{"openid", "profile", "offline_access", "https://database.windows.net/user_impersonation"},
	}
	// Plain http redirect URL means local dev; browsers drop Secure cookies there.
	secureCookies = strings.HasPrefix(oauthCfg.RedirectURL, "https://")
	go func() {
		for now := range time.Tick(time.Minute) {
			sweepSessions(now)
		}
	}()
}

// sessionTTL caps how long a login, and therefore a leaked sid cookie, stays
// usable. The refresh token would otherwise keep minting SQL tokens for up
// to 90 days. Re-login also re-checks group membership at Entra.
const sessionTTL = 12 * time.Hour

// idleTTL drops a session, closing its SQL connections, after this long
// without a request; the sweeper enforces it even if the browser never returns.
const idleTTL = 30 * time.Minute

// session is one login. It is reached only through the sid cookie (withSession),
// and it is the only owner of its token source and its SQL pools: handlers get
// database handles from session.db, never from a shared place, so a request can
// use no other user's connections. TestSessionsKeepTheirOwnPoolsAndTokens pins this.
type session struct {
	Name     string
	Email    string
	expires  time.Time
	lastSeen time.Time // updated by withSession; guarded by sessions.Mutex
	ts       oauth2.TokenSource
	mu       sync.Mutex
	dbs      map[string]*sql.DB    // "server/database" -> pool, filled by sql.go
	jobs     map[string]*importJob // by id, guarded by mu; see import.go
}

// ended says why the session is over: "expired" past sessionTTL, "idle" past
// idleTTL without a request, or "" while it is still alive.
func (s *session) ended(now time.Time) string {
	switch {
	case now.After(s.expires):
		return "expired"
	case now.Sub(s.lastSeen) > idleTTL:
		return "idle"
	}
	return ""
}

// expired reports whether the session passed sessionTTL or sat idle for idleTTL.
func (s *session) expired(now time.Time) bool {
	return s.ended(now) != ""
}

// ponytail: in-memory sessions, single instance. Swap the map for Redis if scaled out.
var sessions = struct {
	sync.Mutex
	m map[string]*session
}{m: map[string]*session{}}

// sweepSessions drops every expired or idle session, closing its SQL pools.
// A goroutine calls it once a minute; withSession also catches expiry on use.
func sweepSessions(now time.Time) {
	dead := map[string]string{} // id -> why
	sessions.Lock()
	for id, s := range sessions.m {
		if why := s.ended(now); why != "" {
			dead[id] = why
		}
	}
	sessions.Unlock()
	for id, why := range dead {
		dropSession(id, why)
	}
}

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
	// Entra replaces the groups claim with a _claim_names/_claim_sources
	// reference when the user is in too many groups (the "overage").
	ClaimNames map[string]json.RawMessage `json:"_claim_names"`
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

// dropSession removes id from sessions.m and closes any DB pools it held,
// logging a logout with why: "user", "idle", "expired" or "relogin".
func dropSession(id, why string) {
	sessions.Lock()
	s := sessions.m[id]
	delete(sessions.m, id)
	sessions.Unlock()
	if s != nil {
		audit(auditEvent{Event: "logout", User: s.Email, Name: s.Name, Reason: why}, nil)
		s.mu.Lock()
		for _, j := range s.jobs {
			j.cancel()
		}
		for _, db := range s.dbs {
			db.Close()
		}
		s.mu.Unlock()
	}
}

// checkClaims verifies audience and tenant, and group membership when group is
// non-empty. An empty group admits every user in the tenant.
func checkClaims(c claims, clientID, tenant, group string) error {
	if c.Aud != clientID {
		return errors.New("id_token audience mismatch")
	}
	if c.Tid != tenant {
		return errors.New("id_token tenant mismatch")
	}
	if group == "" {
		return nil
	}
	for _, g := range c.Groups {
		if g == group {
			return nil
		}
	}
	// Say why the group is missing: a misconfigured app registration is far
	// more common than a user who truly is not in the group.
	if _, ok := c.ClaimNames["groups"]; ok {
		return errors.New("the token's group list overflowed; in the app registration under Token configuration, set the groups claim to \"Groups assigned to the application\" and assign the group to the app")
	}
	if c.Groups == nil {
		return errors.New("the id_token has no groups claim; in the app registration add it under Token configuration > Add groups claim > Security groups")
	}
	return errors.New("not a member of the allowed group (ALLOWED_GROUP_ID must be the group's object ID, and the user a member of that group; a user assigned directly to the app is not)")
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

// auditLogin records a login attempt; cl names the user once the id_token got that far.
func auditLogin(cl claims, err error) {
	audit(auditEvent{Event: "login", User: cl.Email, Name: cl.Name}, err)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie("oauth_state")
	if err != nil || c.Value == "" || c.Value != q.Get("state") {
		auditLogin(claims{}, errors.New("invalid state"))
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		auditLogin(claims{}, errors.New(e+": "+q.Get("error_description")))
		http.Error(w, e+": "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	tok, err := oauthCfg.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		log.Printf("token exchange: %v", err)
		auditLogin(claims{}, fmt.Errorf("token exchange: %w", err))
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	cl, err := parseIDToken(raw)
	if err != nil {
		log.Printf("id_token: %v", err)
		auditLogin(claims{}, fmt.Errorf("id_token: %w", err))
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	if err := checkClaims(cl, oauthCfg.ClientID, tenantID, allowedGroup); err != nil {
		auditLogin(cl, err)
		http.Error(w, "access denied: "+err.Error(), http.StatusForbidden)
		return
	}
	if old, err := r.Cookie("sid"); err == nil {
		dropSession(old.Value, "relogin")
	}
	id := randomID()
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	sessions.Lock()
	sessions.m[id] = &session{
		Name:     cl.Name,
		Email:    cl.Email,
		expires:  time.Now().Add(sessionTTL),
		lastSeen: time.Now(),
		ts:       oauthCfg.TokenSource(ctx, tok),
		dbs:      map[string]*sql.DB{},
	}
	sessions.Unlock()
	auditLogin(cl, nil)
	setCookie(w, "sid", id, "/", 0)
	setCookie(w, "oauth_state", "", "/auth", -1)
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		dropSession(c.Value, "user")
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
		now := time.Now()
		sessions.Lock()
		s := sessions.m[c.Value]
		if s != nil && !s.expired(now) {
			s.lastSeen = now
		}
		sessions.Unlock()
		if s != nil && s.expired(now) {
			dropSession(c.Value, s.ended(now))
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
