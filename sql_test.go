package main

import (
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
