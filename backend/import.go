package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CSV import: POST a file to .../t/{schema}/{table}/csv and a new table is
// created from it. The header row names the columns, and every column gets
// the narrowest type that fits all of its values; values are inserted typed,
// in one transaction with the CREATE TABLE, so a failed import leaves nothing
// behind.

// Type kinds a text value can be, from most to least specific. A column's
// kind is the intersection over its non-empty values; sqlType and sqlValue
// must pick in the same order.
const (
	kBit = 1 << iota
	kInt
	kBigint
	kFloat
	kDate
	kDateTime // no zone -> datetime2
	kDTOffset // with zone -> datetimeoffset
)

// Layouts match the app's own CSV export (style 121 and RFC 3339); trailing
// 9s make the fraction optional.
var (
	dateLayout      = "2006-01-02"
	dateTimeLayouts = []string{"2006-01-02 15:04:05.9999999", "2006-01-02T15:04:05.9999999"}
	dtOffsetLayouts = []string{"2006-01-02 15:04:05.9999999 -07:00", time.RFC3339Nano}
)

func parseAny(layouts []string, v string) (time.Time, bool) {
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// valueKinds reports every kind the text v could be.
func valueKinds(v string) int {
	k := 0
	if strings.EqualFold(v, "true") || strings.EqualFold(v, "false") {
		k |= kBit
	}
	n, ierr := strconv.ParseInt(v, 10, 64)
	if ierr == nil {
		k |= kBigint
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			k |= kInt
		}
	}
	// An integer that overflows bigint (ErrRange) must not become float and
	// silently lose digits; it stays text.
	if !errors.Is(ierr, strconv.ErrRange) {
		if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			k |= kFloat
		}
	}
	if _, err := time.Parse(dateLayout, v); err == nil {
		k |= kDate
	}
	if _, ok := parseAny(dateTimeLayouts, v); ok {
		k |= kDateTime
	}
	if _, ok := parseAny(dtOffsetLayouts, v); ok {
		k |= kDTOffset
	}
	return k
}

// csvColumn is one column of an imported CSV.
type csvColumn struct {
	Name   string
	kinds  int
	maxLen int  // in UTF-16 code units, what nvarchar(n) counts
	seen   bool // any non-empty value
}

// nvarchar sizes are rounded up so the table has room for later, longer values.
var nvarcharSteps = []int{50, 100, 200, 400, 1000, 4000}

func (c csvColumn) sqlType() string {
	if c.seen {
		switch {
		case c.kinds&kBit != 0:
			return "bit"
		case c.kinds&kInt != 0:
			return "int"
		case c.kinds&kBigint != 0:
			return "bigint"
		case c.kinds&kDate != 0:
			return "date"
		case c.kinds&kDateTime != 0:
			return "datetime2"
		case c.kinds&kDTOffset != 0:
			return "datetimeoffset"
		case c.kinds&kFloat != 0:
			return "float"
		}
	}
	for _, n := range nvarcharSteps {
		if c.maxLen <= n {
			return fmt.Sprintf("nvarchar(%d)", n)
		}
	}
	return "nvarchar(max)"
}

// sqlValue converts one field for insertion into this column. Empty is NULL,
// matching CSV export. Inference proved the text parses; if it somehow does
// not, the raw string goes through and SQL Server decides.
func (c csvColumn) sqlValue(v string) any {
	if v == "" {
		return nil
	}
	switch {
	case c.kinds&kBit != 0:
		if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
			return b
		}
	case c.kinds&(kInt|kBigint) != 0:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case c.kinds&kDate != 0:
		if t, err := time.Parse(dateLayout, v); err == nil {
			return t
		}
	case c.kinds&(kDateTime|kDTOffset) != 0:
		if t, ok := parseAny(append(dateTimeLayouts, dtOffsetLayouts...), v); ok {
			return t
		}
	case c.kinds&kFloat != 0:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return v
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// inferColumns names the columns from the header (blanks become columnN,
// duplicates are refused) and narrows each column's kind over all rows.
func inferColumns(header []string, rows [][]string) ([]csvColumn, error) {
	all := kBit | kInt | kBigint | kFloat | kDate | kDateTime | kDTOffset
	cols := make([]csvColumn, len(header))
	names := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			h = fmt.Sprintf("column%d", i+1)
		}
		key := strings.ToLower(h)
		if names[key] {
			return nil, fmt.Errorf("duplicate column name %q", h)
		}
		names[key] = true
		cols[i] = csvColumn{Name: h, kinds: all}
	}
	for _, row := range rows {
		for i, v := range row {
			if v == "" {
				continue
			}
			c := &cols[i]
			c.seen = true
			c.kinds &= valueKinds(v)
			if n := utf16Len(v); n > c.maxLen {
				c.maxLen = n
			}
		}
	}
	return cols, nil
}

func createTableSQL(schema, table string, cols []csvColumn) string {
	defs := make([]string, len(cols))
	for i, c := range cols {
		defs[i] = quoteIdent(c.Name) + " " + c.sqlType() + " NULL"
	}
	return "CREATE TABLE " + quoteIdent(schema) + "." + quoteIdent(table) + " (" + strings.Join(defs, ", ") + ")"
}

// buildImport turns the rows into multi-row INSERTs, staying under SQL
// Server's 2100-parameter and 1000-rows-per-VALUES limits.
func buildImport(schema, table string, cols []csvColumn, rows [][]string) []stmt {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.Name)
	}
	head := "INSERT INTO " + quoteIdent(schema) + "." + quoteIdent(table) + " (" + strings.Join(names, ", ") + ") VALUES "
	per := min(1000, max(1, 2000/len(cols)))
	var out []stmt
	for start := 0; start < len(rows); start += per {
		chunk := rows[start:min(start+per, len(rows))]
		vals := make([]string, len(chunk))
		var args []any
		for j, row := range chunk {
			marks := make([]string, len(cols))
			for i, c := range cols {
				args = append(args, c.sqlValue(row[i]))
				marks[i] = fmt.Sprintf("@p%d", len(args))
			}
			vals[j] = "(" + strings.Join(marks, ", ") + ")"
		}
		out = append(out, stmt{SQL: head + strings.Join(vals, ", "), Args: args, Kind: "insert"})
	}
	return out
}

// sniffDelim guesses the delimiter from the header line: comma unless
// semicolons or tabs are more frequent (Excel writes ';' in many locales).
// ponytail: counts ignore quoting; a comma-heavy first field could mislead.
func sniffDelim(data []byte) rune {
	line := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line = data[:i]
	}
	delim, best := ',', bytes.Count(line, []byte{','})
	for _, d := range []byte{';', '\t'} {
		if n := bytes.Count(line, []byte{d}); n > best {
			delim, best = rune(d), n
		}
	}
	return delim
}

// tableCol is one column of an existing table for append: its name and
// whether an INSERT may set it (identity, computed and rowversion cannot).
type tableCol struct {
	Name     string
	Writable bool
}

// writableColumns lists obj's columns in order; errNotFound if obj does not exist.
func writableColumns(ctx context.Context, db *sql.DB, obj string) ([]tableCol, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.name,
		CASE WHEN c.is_identity = 1 OR c.is_computed = 1 OR TYPE_NAME(c.system_type_id) = 'timestamp' THEN 0 ELSE 1 END
		FROM sys.columns c WHERE c.object_id = OBJECT_ID(@p1) ORDER BY c.column_id`, obj)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableCol
	for rows.Next() {
		var c tableCol
		if err := rows.Scan(&c.Name, &c.Writable); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errNotFound
	}
	return out, nil
}

// matchColumns maps the CSV header onto an existing table's columns,
// case-insensitively. Unwritable columns are skipped (so a downloaded CSV
// imports back; identity values are not kept), unknown ones are refused.
// The returned columns carry no kinds, so sqlValue passes values through as
// text for SQL Server to convert, exactly like grid edits; keep holds the
// header index of each returned column.
func matchColumns(header []string, tcols []tableCol) ([]csvColumn, []int, error) {
	byName := map[string]tableCol{}
	for _, c := range tcols {
		byName[strings.ToLower(c.Name)] = c
	}
	var cols []csvColumn
	var keep []int
	for i, h := range header {
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, nil, fmt.Errorf("column %d has no name", i+1)
		}
		c, ok := byName[strings.ToLower(h)]
		if !ok {
			return nil, nil, fmt.Errorf("the table has no column %q", h)
		}
		if !c.Writable {
			continue
		}
		cols = append(cols, csvColumn{Name: c.Name})
		keep = append(keep, i)
	}
	if len(cols) == 0 {
		return nil, nil, errors.New("no writable columns in the file")
	}
	return cols, keep, nil
}

// project reduces each row to the kept indices.
func project(rows [][]string, keep []int) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		r := make([]string, len(keep))
		for j, k := range keep {
			r[j] = row[k]
		}
		out[i] = r
	}
	return out
}

// handleImportCSV fills [schema].[table] from the posted CSV, all in one
// transaction. Without append=1 the table is created first, its column types
// inferred from the values; with append=1 the rows go into the existing
// table, converted by SQL Server like grid edits.
func handleImportCSV(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 100<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	data = bytes.TrimPrefix(data, []byte("\ufeff")) // Excel's UTF-8 BOM
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma = sniffDelim(data)
	records, err := cr.ReadAll()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad csv: " + err.Error()})
		return
	}
	if len(records) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty file; the first row must name the columns"})
		return
	}
	if len(records[0]) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "more than 1024 columns"})
		return
	}
	header, body := records[0], records[1:]
	var stmts []stmt
	if r.URL.Query().Get("append") == "1" {
		tcols, err := writableColumns(r.Context(), db, objName(r))
		if err != nil {
			fail(w, err)
			return
		}
		cols, keep, err := matchColumns(header, tcols)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		stmts = buildImport(r.PathValue("schema"), r.PathValue("table"), cols, project(body, keep))
	} else {
		cols, err := inferColumns(header, body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		stmts = append([]stmt{{SQL: createTableSQL(r.PathValue("schema"), r.PathValue("table"), cols)}},
			buildImport(r.PathValue("schema"), r.PathValue("table"), cols, body)...)
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(r.Context(), st.SQL, st.Args...); err != nil {
			tx.Rollback()
			fail(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": len(records) - 1})
}
