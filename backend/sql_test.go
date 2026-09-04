package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"golang.org/x/oauth2"
)

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("a]b"); got != "[a]]b]" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildBatch(t *testing.T) {
	stmts, err := buildBatch("dbo", "t", batch{
		Inserts: []map[string]any{{"b": "2", "a": "1"}, {}},
		Updates: []update{{Key: map[string]any{"id": "7"}, Set: map[string]any{"name": "x"}}},
		Deletes: []map[string]any{{"id": "9"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []stmt{
		{SQL: "INSERT INTO [dbo].[t] ([a], [b]) VALUES (@p1, @p2)", Args: []any{"1", "2"}, Kind: "insert"},
		{SQL: "INSERT INTO [dbo].[t] DEFAULT VALUES", Kind: "insert"},
		{SQL: "UPDATE [dbo].[t] SET [name] = @p1 WHERE [id] = @p2", Args: []any{"x", "7"}, Kind: "update"},
		{SQL: "DELETE FROM [dbo].[t] WHERE [id] = @p1", Args: []any{"9"}, Kind: "delete"},
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got %#v\nwant %#v", stmts, want)
	}
}

func TestBuildBatchRejectsEmptyKey(t *testing.T) {
	if _, err := buildBatch("dbo", "t", batch{Deletes: []map[string]any{{}}}); err == nil {
		t.Fatal("delete without key accepted")
	}
	if _, err := buildBatch("dbo", "t", batch{Updates: []update{{Set: map[string]any{"a": "1"}}}}); err == nil {
		t.Fatal("update without key accepted")
	}
}

func TestParseServers(t *testing.T) {
	m, names, err := parseServers("sqlserver://a.internal:1433?encrypt=true, sqlserver://b?trustservercertificate=true")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"a.internal", "b"}) {
		t.Fatalf("names %v", names)
	}
	if m["a.internal"].Port != 1433 || m["b"].Host != "b" {
		t.Fatalf("configs %+v", m)
	}
	if _, _, err := parseServers(""); err == nil {
		t.Fatal("empty list accepted")
	}
	if _, _, err := parseServers("sqlserver://a.internal:1433,sqlserver://a.internal:1433"); err == nil {
		t.Fatal("duplicate host accepted")
	}
}

func TestJSONValue(t *testing.T) {
	if got := jsonValue([]byte("12.50"), "DECIMAL"); got != "12.50" {
		t.Fatalf("decimal: %v", got)
	}
	guid := []byte{0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	if got := jsonValue(guid, "UNIQUEIDENTIFIER"); got != "00112233-4455-6677-8899-AABBCCDDEEFF" {
		t.Fatalf("guid: %v", got)
	}
	if got := jsonValue([]byte{1, 2}, "VARBINARY"); got != "AQI=" {
		t.Fatalf("binary: %v", got)
	}
	if got := jsonValue(int64(3), "INT"); got != int64(3) {
		t.Fatalf("int: %v", got)
	}
	if got := jsonValue(nil, "INT"); got != nil {
		t.Fatalf("nil: %v", got)
	}
}

// TestDevModeDBIsCachedWithoutTokenSource guards that session.db with a nil
// TokenSource (dev mode) returns a pool without error and caches it under
// "server/database". It does NOT distinguish the SQL-login connector from the
// token connector: mssql.NewSecurityTokenConnector only stores its closure
// and sql.OpenDB is lazy, so both paths return a non-nil *sql.DB here without
// dialing anything. Which connector actually gets used is only observable at
// login time against a live server, so that choice is covered by the Task 8
// Step 7 integration smoke test, not by this test.
func TestDevModeDBIsCachedWithoutTokenSource(t *testing.T) {
	m, names, err := parseServers("sqlserver://sa:secret@localhost:1433")
	if err != nil {
		t.Fatal(err)
	}
	servers, serverNames = m, names
	defer func() { servers, serverNames = nil, nil }()
	s := &session{dbs: map[string]*sql.DB{}} // ts == nil means dev mode
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the wake ping must bail before dialing; the handle is still cached
	if _, err := s.db(ctx, "localhost", "master"); !errors.Is(err, context.Canceled) {
		t.Fatalf("db: want context.Canceled, got %v", err)
	}
	db := s.dbs["localhost/master"]
	if db == nil {
		t.Fatal("pool not cached")
	}
	db.Close()
}

// fakeConnector fails the first `fails` connects with the given SQL error number.
type fakeConnector struct {
	fails, calls int
	number       int32
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, mssql.Error{Number: f.number}
	}
	return fakeConn{}, nil
}
func (f *fakeConnector) Driver() driver.Driver { return nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func TestWakeRetriesWhileResuming(t *testing.T) {
	wakeRetry = time.Millisecond
	for _, c := range []struct {
		number    int32
		wantCalls int
		wantErr   bool
	}{
		{40613, 3, false}, // paused serverless database: retry until it is up
		{18456, 1, true},  // login failed: give up at once
	} {
		fc := &fakeConnector{fails: 2, number: c.number}
		d := sql.OpenDB(fc)
		err := wake(context.Background(), d)
		d.Close()
		if (err != nil) != c.wantErr || fc.calls != c.wantCalls {
			t.Errorf("error %d: err=%v calls=%d, want err=%v calls=%d", c.number, err, fc.calls, c.wantErr, c.wantCalls)
		}
	}
}

func TestFailStatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{"not found", errNotFound, 404, ""},
		{"sql error", mssql.Error{Message: "boom"}, 400, "boom"},
		{"token expired", &oauth2.RetrieveError{}, 401, ""},
		{"other", errors.New("x"), 500, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fail(rec, c.err)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if c.wantBody != "" && !strings.Contains(rec.Body.String(), c.wantBody) {
				t.Fatalf("body %q does not contain %q", rec.Body.String(), c.wantBody)
			}
		})
	}
}

func TestSearchWhere(t *testing.T) {
	cols := []string{"id", "name", "photo"}
	types := []string{"INT", "NVARCHAR", "VARBINARY"}
	like := func(col string, n int) string {
		return "CAST([" + col + "] AS nvarchar(max)) LIKE @p" + string(rune('0'+n)) + " ESCAPE '\\'"
	}
	where, args, err := searchWhere(cols, types, "al%an ID=3")
	if err != nil {
		t.Fatal(err)
	}
	want := " WHERE [id] = @p2 AND ((" + like("id", 1) + ") OR (" + like("name", 1) + "))"
	if where != want {
		t.Fatalf("got  %s\nwant %s", where, want)
	}
	if !reflect.DeepEqual(args, []any{`%al\%an%`, "3"}) {
		t.Fatalf("args %#v", args)
	}
	// Two bare words must occur in the same column.
	where, _, _ = searchWhere(cols, types, "User 1001")
	want = " WHERE ((" + like("id", 1) + " AND " + like("id", 2) + ") OR (" + like("name", 1) + " AND " + like("name", 2) + "))"
	if where != want {
		t.Fatalf("two words:\ngot  %s\nwant %s", where, want)
	}
	// Quotes keep spaces (and =) together; an empty value is allowed.
	where, args, err = searchWhere(cols, types, `name='User 1002' "two words" 'a=b' id=`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []any{"User 1002", "%two words%", "%a=b%", ""}) || !strings.HasPrefix(where, " WHERE [name] = @p1 AND [id] = @p4 AND (") {
		t.Fatalf("quoted: %s %#v", where, args)
	}
	if where, args, err := searchWhere(cols, types, "  "); where != "" || args != nil || err != nil {
		t.Fatalf("blank: %q %v %v", where, args, err)
	}
	if _, _, err := searchWhere(cols, types, "nope=1"); err == nil {
		t.Fatal("unknown column accepted")
	}
	if _, _, err := searchWhere([]string{"photo"}, []string{"VARBINARY"}, "x"); err == nil {
		t.Fatal("free text over unsearchable columns accepted")
	}
}
