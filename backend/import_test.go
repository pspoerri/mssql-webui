package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInferColumns(t *testing.T) {
	header := []string{"flag", "small", "big", "huge", "ratio", "day", "stamp", "zoned", "note", " ", "empty"}
	rows := [][]string{
		{"true", "1", "3000000000", "99999999999999999999", "1.5", "2024-01-02", "2024-01-02 03:04:05", "2024-01-02T03:04:05.5+01:00", "hi", "x", ""},
		{"FALSE", "-2", "7", "1", "2", "2023-12-31", "2024-01-02 03:04:05.1234567", "2024-01-02 03:04:05.5 +01:00", strings.Repeat("ä", 60), "", ""},
		{"", "", "", "", "", "", "", "", "", "", ""},
	}
	cols, err := inferColumns(header, rows)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bit", "int", "bigint", "nvarchar(50)", "float", "date", "datetime2", "datetimeoffset", "nvarchar(100)", "nvarchar(50)", "nvarchar(50)"}
	for i, w := range want {
		if got := cols[i].sqlType(); got != w {
			t.Errorf("%s: got %s, want %s", cols[i].Name, got, w)
		}
	}
	if cols[9].Name != "column10" {
		t.Errorf("blank header: %q", cols[9].Name)
	}
	if _, err := inferColumns([]string{"a", "A"}, nil); err == nil {
		t.Fatal("duplicate column accepted")
	}
}

func TestSQLValue(t *testing.T) {
	cols, err := inferColumns([]string{"b", "n", "f", "d", "ts", "s"},
		[][]string{{"true", "3", "1.5", "2024-01-02", "2024-01-02 03:04:05", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	vals := make([]any, len(cols))
	for i, v := range []string{"false", "-7", "2.5", "2024-02-03", "2024-01-02 03:04:05.5", ""} {
		vals[i] = cols[i].sqlValue(v)
	}
	want := []any{false, int64(-7), 2.5,
		time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 3, 4, 5, 500000000, time.UTC), nil}
	if !reflect.DeepEqual(vals, want) {
		t.Fatalf("got %#v\nwant %#v", vals, want)
	}
}

func TestCreateTableSQL(t *testing.T) {
	cols, _ := inferColumns([]string{"id", "name"}, [][]string{{"1", "x"}})
	got := createTableSQL("dbo", "t", cols)
	want := "CREATE TABLE [dbo].[t] ([id] int NULL, [name] nvarchar(50) NULL)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestBuildImport(t *testing.T) {
	cols, _ := inferColumns([]string{"a", "b"}, [][]string{{"1", "x"}})
	stmts := buildImport("dbo", "t", cols, [][]string{{"1", "x"}, {"", "y"}})
	if len(stmts) != 1 {
		t.Fatalf("stmts %d", len(stmts))
	}
	want := "INSERT INTO [dbo].[t] ([a], [b]) VALUES (@p1, @p2), (@p3, @p4)"
	if stmts[0].SQL != want {
		t.Fatalf("got %s", stmts[0].SQL)
	}
	if !reflect.DeepEqual(stmts[0].Args, []any{int64(1), "x", nil, "y"}) {
		t.Fatalf("args %#v", stmts[0].Args)
	}
	// 3 columns -> 666 rows per statement; 1400 rows need 3 statements.
	cols3, _ := inferColumns([]string{"a", "b", "c"}, nil)
	rows := make([][]string, 1400)
	for i := range rows {
		rows[i] = []string{"1", "2", "3"}
	}
	if got := len(buildImport("dbo", "t", cols3, rows)); got != 3 {
		t.Fatalf("batches %d", got)
	}
}

func TestMatchColumns(t *testing.T) {
	tcols := []tableCol{{Name: "Id", Writable: false}, {Name: "Name", Writable: true}, {Name: "Age", Writable: true}}
	cols, keep, err := matchColumns([]string{"id", " NAME ", "age"}, tcols)
	if err != nil {
		t.Fatal(err)
	}
	// id is identity and skipped; the table's own spelling is used.
	if len(cols) != 2 || cols[0].Name != "Name" || cols[1].Name != "Age" {
		t.Fatalf("cols %#v", cols)
	}
	if !reflect.DeepEqual(keep, []int{1, 2}) {
		t.Fatalf("keep %v", keep)
	}
	// Matched columns carry no kinds: values pass through as text, empty is NULL.
	if v := cols[0].sqlValue("123"); v != "123" {
		t.Fatalf("value %#v", v)
	}
	if v := cols[0].sqlValue(""); v != nil {
		t.Fatalf("empty %#v", v)
	}
	if _, _, err := matchColumns([]string{"nope"}, tcols); err == nil {
		t.Fatal("unknown column accepted")
	}
	if _, _, err := matchColumns([]string{"name", ""}, tcols); err == nil {
		t.Fatal("blank header accepted")
	}
	if _, _, err := matchColumns([]string{"id"}, tcols); err == nil {
		t.Fatal("identity-only file accepted")
	}
	rows := project([][]string{{"1", "a", "9"}, {"2", "b", "8"}}, keep)
	if !reflect.DeepEqual(rows, [][]string{{"a", "9"}, {"b", "8"}}) {
		t.Fatalf("project %v", rows)
	}
}

func TestSniffDelim(t *testing.T) {
	cases := map[string]rune{
		"a,b,c\n1,2,3":  ',',
		"a;b;c\n1;2;3":  ';',
		"a\tb\tc\n1\t2": '\t',
		"a;b,c;d\nx":    ';',
		"single":        ',',
	}
	for in, want := range cases {
		if got := sniffDelim([]byte(in)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
