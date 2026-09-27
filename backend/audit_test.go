package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"golang.org/x/oauth2"
)

// captureAudit sends the audit log into a buffer for this test and returns a
// function that parses and clears the lines written so far.
func captureAudit(t *testing.T) func() []auditEvent {
	t.Helper()
	var buf bytes.Buffer
	auditOut.SetOutput(&buf)
	t.Cleanup(func() { auditOut.SetOutput(os.Stdout) })
	return func() []auditEvent {
		var out []auditEvent
		for _, line := range strings.Split(buf.String(), "\n") {
			if line == "" {
				continue
			}
			var e auditEvent
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("audit line %q: %v", line, err)
			}
			out = append(out, e)
		}
		buf.Reset()
		return out
	}
}

// stmtConn is a fake connection whose statements succeed unless their text
// says otherwise: "fail" is refused at once, "streamfail" errors after the
// rows, the way a division by zero arrives from SQL Server.
type stmtConn struct{}

func (stmtConn) Prepare(q string) (driver.Stmt, error) { return stmtFake{q}, nil }
func (stmtConn) Close() error                          { return nil }
func (stmtConn) Begin() (driver.Tx, error)             { return txFake{}, nil }

type stmtFake struct{ q string }

func (stmtFake) Close() error                               { return nil }
func (stmtFake) NumInput() int                              { return -1 }
func (stmtFake) Exec([]driver.Value) (driver.Result, error) { return nil, errors.New("unused") }
func (stmtFake) Query([]driver.Value) (driver.Rows, error)  { return nil, errors.New("unused") }

func (s stmtFake) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(s.q, "fail") {
		return nil, errors.New("boom")
	}
	return driver.RowsAffected(2), nil
}

func (s stmtFake) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(s.q, "fail") && !strings.Contains(s.q, "streamfail") {
		return nil, errors.New("boom")
	}
	return &rowsFake{left: 3, streamErr: strings.Contains(s.q, "streamfail")}, nil
}

type rowsFake struct {
	left      int
	streamErr bool
}

func (r *rowsFake) Columns() []string { return []string{"n"} }
func (r *rowsFake) Close() error      { return nil }
func (r *rowsFake) Next(dest []driver.Value) error {
	if r.left == 0 {
		if r.streamErr {
			return errors.New("late boom")
		}
		return io.EOF
	}
	r.left--
	dest[0] = int64(r.left)
	return nil
}

type txFake struct{}

func (txFake) Commit() error   { return nil }
func (txFake) Rollback() error { return nil }

type stmtConnector struct{}

func (stmtConnector) Connect(context.Context) (driver.Conn, error) { return stmtConn{}, nil }
func (stmtConnector) Driver() driver.Driver                        { return nil }

func TestAuditLogsEveryStatement(t *testing.T) {
	events := captureAudit(t)
	db := sql.OpenDB(auditConnector{Connector: stmtConnector{}, base: auditEvent{User: "ann@example.com", Server: "sql1", DB: "app"}})
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "UPDATE t SET a = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET fail = 1"); err == nil {
		t.Fatal("fake did not fail")
	}
	rows, err := db.QueryContext(ctx, "SELECT n FROM t")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	if _, err := db.QueryContext(ctx, "SELECT fail"); err == nil {
		t.Fatal("fake did not fail")
	}
	rows, err = db.QueryContext(ctx, "SELECT streamfail")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if rows.Err() == nil {
		t.Fatal("stream error lost")
	}
	rows.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM t"); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	tx, _ = db.BeginTx(ctx, nil)
	tx.Rollback()
	bulk, err := db.PrepareContext(ctx, `INSERTBULK {"TableName":"t"}`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := bulk.ExecContext(ctx, i); err != nil { // one row each: not logged
			t.Fatal(err)
		}
	}
	if _, err := bulk.ExecContext(ctx); err != nil { // the flush: logged once
		t.Fatal(err)
	}
	bulk.Close()
	long := "INSERT " + strings.Repeat("é", maxAuditSQL)
	if _, err := db.ExecContext(ctx, long); err != nil {
		t.Fatal(err)
	}

	two, three := int64(2), int64(3)
	want := []auditEvent{
		{Event: "connect", OK: true},
		{Event: "exec", SQL: "UPDATE t SET a = 1", OK: true, Rows: &two},
		{Event: "exec", SQL: "UPDATE t SET fail = 1", Error: "boom"},
		{Event: "query", SQL: "SELECT n FROM t", OK: true, Rows: &three},
		{Event: "query", SQL: "SELECT fail", Error: "boom"},
		{Event: "query", SQL: "SELECT streamfail", Error: "late boom", Rows: &three},
		{Event: "begin", OK: true},
		{Event: "exec", SQL: "DELETE FROM t", OK: true, Rows: &two},
		{Event: "commit", OK: true},
		{Event: "begin", OK: true},
		{Event: "rollback", OK: true},
		{Event: "exec", SQL: `INSERTBULK {"TableName":"t"}`, OK: true, Rows: &two},
		// Split over lines, each cut before a split rune: 7 + 16384 bytes -> 8191, 8192, 8
		{Event: "exec", SQL: long[:maxAuditSQL-1], OK: true, Rows: &two, Part: 1, Parts: 3},
		{Event: "exec", SQL: long[maxAuditSQL-1 : 2*maxAuditSQL-1], OK: true, Rows: &two, Part: 2, Parts: 3},
		{Event: "exec", SQL: long[2*maxAuditSQL-1:], OK: true, Rows: &two, Part: 3, Parts: 3},
	}
	got := events()
	if n := len(got); n >= 3 && (got[n-1].ID == "" || got[n-1].ID != got[n-3].ID || got[n-2].SQL+got[n-1].SQL != long[maxAuditSQL-1:]) {
		t.Errorf("parts do not share an id or do not reassemble: %q %q %q", got[n-3].ID, got[n-2].ID, got[n-1].ID)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d:\n%+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.User != "ann@example.com" || g.Server != "sql1" || g.DB != "app" || g.Time == "" {
			t.Errorf("event %d: identity/time missing: %+v", i, g)
		}
		rowsMatch := (g.Rows == nil) == (w.Rows == nil) && (g.Rows == nil || *g.Rows == *w.Rows)
		if g.Event != w.Event || g.SQL != w.SQL || g.OK != w.OK || g.Error != w.Error || g.Part != w.Part || g.Parts != w.Parts || !rowsMatch {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// A refused connect is logged with the server's message; the next one succeeds.
func TestAuditLogsConnectFailures(t *testing.T) {
	events := captureAudit(t)
	fc := &fakeConnector{fails: 1, number: 18456}
	db := sql.OpenDB(auditConnector{Connector: fc, base: auditEvent{User: "bob@example.com", Server: "sql1", DB: "secret"}})
	defer db.Close()
	if err := db.PingContext(context.Background()); err == nil {
		t.Fatal("first connect should fail")
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := events()
	if len(got) != 2 || got[0].Event != "connect" || got[0].OK || got[1].Event != "connect" || !got[1].OK {
		t.Fatalf("events %+v", got)
	}
	if got[0].DB != "secret" || got[0].User != "bob@example.com" {
		t.Fatalf("identity: %+v", got[0])
	}
}

// The real driver's connection type must keep its optional interfaces behind
// the wrapper, or parameter conversion and pooling would silently change.
func TestAuditConnForwardsDriverInterfaces(t *testing.T) {
	var c driver.Conn = &auditConn{Conn: &mssql.Conn{}}
	for name, ok := range map[string]bool{
		"NamedValueChecker":  func() bool { _, ok := c.(driver.NamedValueChecker); return ok }(),
		"Validator":          func() bool { _, ok := c.(driver.Validator); return ok }(),
		"SessionResetter":    func() bool { _, ok := c.(driver.SessionResetter); return ok }(),
		"Pinger":             func() bool { _, ok := c.(driver.Pinger); return ok }(),
		"ConnBeginTx":        func() bool { _, ok := c.(driver.ConnBeginTx); return ok }(),
		"ConnPrepareContext": func() bool { _, ok := c.(driver.ConnPrepareContext); return ok }(),
	} {
		if !ok {
			t.Errorf("auditConn lacks driver.%s", name)
		}
	}
	var r driver.Rows = &auditRows{Rows: &mssql.Rows{}}
	if _, ok := r.(driver.RowsColumnTypeDatabaseTypeName); !ok {
		t.Error("auditRows lacks ColumnTypeDatabaseTypeName")
	}
	if _, ok := r.(driver.RowsNextResultSet); !ok {
		t.Error("auditRows lacks RowsNextResultSet")
	}
}

func TestAuditLoginAndLogout(t *testing.T) {
	events := captureAudit(t)
	idToken := fakeIDToken(map[string]any{"aud": "cid", "tid": "t1", "name": "Ann", "preferred_username": "ann@example.com"})
	entra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm(); r.Form.Get("code_verifier") != "verifier" { // PKCE: the verifier from the cookie must reach Entra
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at","token_type":"Bearer","refresh_token":"rt","expires_in":3600,"id_token":%q}`, idToken)
	}))
	defer entra.Close()
	oauthCfg = &oauth2.Config{ClientID: "cid", ClientSecret: "s", Endpoint: oauth2.Endpoint{TokenURL: entra.URL}}
	tenantID, allowedGroup = "t1", ""
	defer func() { oauthCfg, tenantID, allowedGroup = nil, "", "" }()

	callback := func(sid string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/auth/callback?code=c&state=st", nil)
		r.AddCookie(&http.Cookie{Name: "oauth_state", Value: "st.verifier"})
		if sid != "" {
			r.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		}
		rec := httptest.NewRecorder()
		handleCallback(rec, r)
		return rec
	}
	sidOf := func(rec *httptest.ResponseRecorder) string {
		for _, c := range rec.Result().Cookies() {
			if c.Name == "sid" && c.Value != "" {
				return c.Value
			}
		}
		t.Fatalf("no sid cookie: %d %s", rec.Code, rec.Body.String())
		return ""
	}

	// A callback without the state cookie is refused and recorded without a user.
	rec := httptest.NewRecorder()
	handleCallback(rec, httptest.NewRequest("GET", "/auth/callback?code=c&state=st", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state: %d", rec.Code)
	}
	// Login, then log in again from the same browser: the old session ends as "relogin".
	sid1 := sidOf(callback(""))
	sid2 := sidOf(callback(sid1))
	// Refused by the group check: recorded with the user who was refused.
	allowedGroup = "g1"
	if rec := callback(""); rec.Code != http.StatusForbidden {
		t.Fatalf("group check: %d", rec.Code)
	}
	allowedGroup = ""
	// The logout button.
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: sid2})
	handleLogout(httptest.NewRecorder(), r)

	want := []auditEvent{
		{Event: "login", Error: "invalid state"},
		{Event: "login", User: "ann@example.com", Name: "Ann", OK: true},
		{Event: "logout", User: "ann@example.com", Name: "Ann", Reason: "relogin", OK: true},
		{Event: "login", User: "ann@example.com", Name: "Ann", OK: true},
		{Event: "login", User: "ann@example.com", Name: "Ann", Error: "no groups claim"},
		{Event: "logout", User: "ann@example.com", Name: "Ann", Reason: "user", OK: true},
	}
	got := events()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d:\n%+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Event != w.Event || g.User != w.User || g.Name != w.Name || g.Reason != w.Reason || g.OK != w.OK || !strings.Contains(g.Error, w.Error) || g.Time == "" {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, g, w)
		}
	}

	// Sessions the sweeper or a request finds over say why they ended.
	now := time.Now()
	sessions.Lock()
	sessions.m["idle"] = &session{Email: "idle@example.com", expires: now.Add(time.Hour), lastSeen: now.Add(-idleTTL - time.Second), dbs: map[string]*sql.DB{}}
	sessions.m["old"] = &session{Email: "old@example.com", expires: now.Add(-time.Second), lastSeen: now, dbs: map[string]*sql.DB{}}
	sessions.m["stale"] = &session{Email: "stale@example.com", expires: now.Add(-time.Second), lastSeen: now, dbs: map[string]*sql.DB{}}
	sessions.Unlock()
	sweepSessions(now)
	r = httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: "stale"})
	withSession(func(http.ResponseWriter, *http.Request, *session) { t.Fatal("stale session served") })(httptest.NewRecorder(), r)
	// Wait: the sweeper already dropped "stale" (it is expired), so the request finds no session.
	reasons := map[string]string{}
	for _, e := range events() {
		if e.Event != "logout" {
			t.Errorf("unexpected %+v", e)
		}
		reasons[e.User] = e.Reason
	}
	if reasons["idle@example.com"] != "idle" || reasons["old@example.com"] != "expired" || reasons["stale@example.com"] != "expired" {
		t.Errorf("reasons %v", reasons)
	}
}

// A session that a request finds expired is logged with why, on that request.
func TestAuditLogoutOnUse(t *testing.T) {
	events := captureAudit(t)
	sessions.Lock()
	sessions.m["lapsed"] = &session{Email: "lapsed@example.com", expires: time.Now().Add(time.Hour), lastSeen: time.Now().Add(-idleTTL - time.Second), dbs: map[string]*sql.DB{}}
	sessions.Unlock()
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: "lapsed"})
	rec := httptest.NewRecorder()
	withSession(func(http.ResponseWriter, *http.Request, *session) { t.Fatal("lapsed session served") })(rec, r)
	got := events()
	if rec.Code != http.StatusUnauthorized || len(got) != 1 || got[0].Event != "logout" || got[0].User != "lapsed@example.com" || got[0].Reason != "idle" {
		t.Fatalf("code %d events %+v", rec.Code, got)
	}
}
