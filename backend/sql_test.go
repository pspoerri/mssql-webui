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
	if got := jsonValue(int64(9007199254740993), "BIGINT"); got != "9007199254740993" {
		t.Fatalf("bigint: %v", got)
	}
	if got := jsonValue(nil, "INT"); got != nil {
		t.Fatalf("nil: %v", got)
	}
	ts := time.Date(2024, 1, 2, 3, 4, 5, 600000000, time.FixedZone("", 2*3600))
	for typ, want := range map[string]string{
		"DATE": "2024-01-02", "TIME": "03:04:05.6", "DATETIME": "2024-01-02 03:04:05.6",
		"DATETIME2": "2024-01-02 03:04:05.6", "DATETIMEOFFSET": "2024-01-02 03:04:05.6 +02:00",
	} {
		if got := jsonValue(ts, typ); got != want {
			t.Errorf("%s: got %v, want %v", typ, got, want)
		}
	}
	if got := jsonValue(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "DATETIME"); got != "2024-01-02 00:00:00" {
		t.Errorf("whole seconds: %v", got)
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

// Two logins get separate pools, and each pool's token callback answers from its own session.
func TestSessionsKeepTheirOwnPoolsAndTokens(t *testing.T) {
	m, names, err := parseServers("sqlserver://localhost:1433")
	if err != nil {
		t.Fatal(err)
	}
	servers, serverNames = m, names
	defer func() { servers, serverNames = nil, nil }()
	ann := &session{Name: "Ann", ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token-ann"}), dbs: map[string]*sql.DB{}}
	bob := &session{Name: "Bob", ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token-bob"}), dbs: map[string]*sql.DB{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the wake ping must bail before dialing; the handles are still cached
	for _, s := range []*session{ann, bob} {
		if _, err := s.db(ctx, "localhost", "master"); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: want context.Canceled, got %v", s.Name, err)
		}
		defer s.dbs["localhost/master"].Close()
	}
	if ann.dbs["localhost/master"] == bob.dbs["localhost/master"] {
		t.Fatal("sessions share a pool")
	}
	if tok, _ := ann.token(ctx); tok != "token-ann" {
		t.Fatalf("ann's connections would log in with %q", tok)
	}
	if tok, _ := bob.token(ctx); tok != "token-bob" {
		t.Fatalf("bob's connections would log in with %q", tok)
	}
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
	like := func(col string, n int) string { // char columns are used as-is, others are CAST
		expr := "[" + col + "]"
		if col != "name" {
			expr = "CAST(" + expr + " AS nvarchar(max))"
		}
		return expr + " LIKE @p" + string(rune('0'+n)) + " ESCAPE '\\'"
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
	// Prefix and contains operators; char columns are not CAST so an index can help.
	where, args, err = searchWhere(cols, types, "name^Us id~00")
	if err != nil {
		t.Fatal(err)
	}
	want = ` WHERE [name] LIKE @p1 ESCAPE '\' AND CAST([id] AS nvarchar(max)) LIKE @p2 ESCAPE '\'`
	if where != want || !reflect.DeepEqual(args, []any{"Us%", "%00%"}) {
		t.Fatalf("ops: %s %#v", where, args)
	}
	where, _, _ = searchWhere([]string{"t"}, []string{"TIME"}, "t^00:10")
	if want := ` WHERE CONVERT(nvarchar(max), [t], 121) LIKE @p1 ESCAPE '\'`; where != want {
		t.Fatalf("time: %s", where)
	}
	for typ, want := range map[string]string{
		"BIT": "CASE [c] WHEN 1 THEN 'true' WHEN 0 THEN 'false' END", "MONEY": "CONVERT(nvarchar(max), [c], 2)",
		"NVARCHAR": "[c]", "INT": "CAST([c] AS nvarchar(max))", "DATE": "CONVERT(nvarchar(max), [c], 121)",
	} {
		if got := textExpr("c", typ); got != want {
			t.Errorf("textExpr %s: %s", typ, got)
		}
	}
	if _, args, err := searchWhere(cols, types, "photo=AQI="); err != nil || !reflect.DeepEqual(args, []any{[]byte{1, 2}}) {
		t.Fatalf("binary =: %v %#v", err, args)
	}
	if _, _, err := searchWhere(cols, types, "photo=nope!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, _, err := searchWhere(cols, types, "photo^ab"); err == nil {
		t.Fatal("prefix on binary column accepted")
	}
	if _, _, err := searchWhere(cols, types, "nope=1"); err == nil {
		t.Fatal("unknown column accepted")
	}
	if _, _, err := searchWhere([]string{"photo"}, []string{"VARBINARY"}, "x"); err == nil {
		t.Fatal("free text over unsearchable columns accepted")
	}
}

func TestOrderBy(t *testing.T) {
	cols := []string{"id", "name"}
	cases := []struct{ sort, dir, want string }{
		{"", "", "[id]"},
		{"NAME", "desc", "[name] DESC, [id]"},
		{"id", "desc", "[id] DESC"},
	}
	for _, c := range cases {
		got, err := orderBy(cols, []string{"id"}, c.sort, c.dir)
		if err != nil || got != c.want {
			t.Errorf("%q/%q: got %q %v, want %q", c.sort, c.dir, got, err, c.want)
		}
	}
	if got, _ := orderBy(cols, nil, "", ""); got != "(SELECT NULL)" {
		t.Errorf("no pk: %q", got)
	}
	if _, err := orderBy(cols, nil, "nope", ""); err == nil {
		t.Error("unknown sort column accepted")
	}
}

func TestTypeLabel(t *testing.T) {
	cases := []struct {
		name, base          string
		maxLen, prec, scale int
		want                string
	}{
		{"nvarchar", "nvarchar", 100, 0, 0, "nvarchar(50)"},
		{"nvarchar", "nvarchar", -1, 0, 0, "nvarchar(max)"},
		{"varchar", "varchar", 20, 0, 0, "varchar(20)"},
		{"decimal", "decimal", 9, 10, 2, "decimal(10,2)"},
		{"datetime2", "datetime2", 8, 27, 7, "datetime2(7)"},
		{"int", "int", 4, 10, 0, "int"},
		{"sysname", "nvarchar", 256, 0, 0, "sysname"},
		{"geometry", "geometry", -1, 0, 0, "geometry"},
	}
	for _, c := range cases {
		if got := typeLabel(c.name, c.base, c.maxLen, c.prec, c.scale); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
