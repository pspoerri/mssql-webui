package main

import (
	"database/sql"
	"reflect"
	"testing"
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
		{SQL: "INSERT INTO [dbo].[t] ([a], [b]) VALUES (@p1, @p2)", Args: []any{"1", "2"}},
		{SQL: "INSERT INTO [dbo].[t] DEFAULT VALUES"},
		{SQL: "UPDATE [dbo].[t] SET [name] = @p1 WHERE [id] = @p2", Args: []any{"x", "7"}},
		{SQL: "DELETE FROM [dbo].[t] WHERE [id] = @p1", Args: []any{"9"}},
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

func TestDevModeUsesSQLLogin(t *testing.T) {
	m, names, err := parseServers("sqlserver://sa:secret@localhost:1433")
	if err != nil {
		t.Fatal(err)
	}
	servers, serverNames = m, names
	defer func() { servers, serverNames = nil, nil }()
	s := &session{dbs: map[string]*sql.DB{}} // ts == nil means dev mode
	db, err := s.db("localhost", "master")
	if err != nil || db == nil {
		t.Fatalf("db: %v", err)
	}
	if s.dbs["localhost/master"] != db {
		t.Fatal("pool not cached")
	}
}
