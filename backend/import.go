package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
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

// newColumns names the columns from the header (blanks become columnN,
// duplicates are refused), each starting as every kind at once.
func newColumns(header []string) ([]csvColumn, error) {
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
	return cols, nil
}

// observeRow narrows each column's kind by one row's values.
func observeRow(cols []csvColumn, row []string) {
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

// inferColumns names the columns from the header and narrows each column's
// kind over all rows.
func inferColumns(header []string, rows [][]string) ([]csvColumn, error) {
	cols, err := newColumns(header)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		observeRow(cols, row)
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

// insertHead is the shared prefix of every INSERT for these columns.
func insertHead(schema, table string, cols []csvColumn) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.Name)
	}
	return "INSERT INTO " + quoteIdent(schema) + "." + quoteIdent(table) + " (" + strings.Join(names, ", ") + ") VALUES "
}

// rowsPerInsert stays under SQL Server's 2100-parameter and
// 1000-rows-per-VALUES limits.
func rowsPerInsert(cols []csvColumn) int {
	return min(1000, max(1, 2000/len(cols)))
}

// insertStmt turns one chunk of rows into a multi-row INSERT.
func insertStmt(head string, cols []csvColumn, chunk [][]string) stmt {
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
	return stmt{SQL: head + strings.Join(vals, ", "), Args: args, Kind: "insert"}
}

// buildImport turns the rows into multi-row INSERTs.
func buildImport(schema, table string, cols []csvColumn, rows [][]string) []stmt {
	head := insertHead(schema, table, cols)
	per := rowsPerInsert(cols)
	var out []stmt
	for start := 0; start < len(rows); start += per {
		out = append(out, insertStmt(head, cols, rows[start:min(start+per, len(rows))]))
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

// projectRow reduces one row to the kept indices.
func projectRow(row []string, keep []int) []string {
	r := make([]string, len(keep))
	for j, k := range keep {
		r[j] = row[k]
	}
	return r
}

// project reduces each row to the kept indices.
func project(rows [][]string, keep []int) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = projectRow(row, keep)
	}
	return out
}

// maxImportBytes caps an upload. The body is spooled to disk, not memory,
// so the cap only protects the temp filesystem.
const maxImportBytes = 2 << 30

// openCSV starts reading the spooled file from the top: past Excel's UTF-8
// BOM, with the sniffed delimiter.
func openCSV(f *os.File, delim rune) (*csv.Reader, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\ufeff")) {
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.Comma = delim
	return cr, nil
}

// handleImportCSV fills [schema].[table] from the posted CSV, all in one
// transaction. Without append=1 the table is created first, its column types
// inferred from the values; with append=1 the rows go into the existing
// table, converted by SQL Server like grid edits.
//
// The body is spooled to a temp file and the CSV is then streamed from it \u2014
// once to infer types (when creating), once to insert in batches \u2014 so memory
// stays flat no matter the file size.
func handleImportCSV(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	tmp, err := os.CreateTemp("", "mssql-webui-import-*.csv")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxImportBytes)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	head := make([]byte, 64<<10)
	n, err := tmp.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		fail(w, err)
		return
	}
	delim := sniffDelim(bytes.TrimPrefix(head[:n], []byte("\ufeff")))

	badCSV := func(err error) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad csv: " + err.Error()})
	}
	cr, err := openCSV(tmp, delim)
	if err != nil {
		fail(w, err)
		return
	}
	header, err := cr.Read()
	if err == io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty file; the first row must name the columns"})
		return
	}
	if err != nil {
		badCSV(err)
		return
	}
	if len(header) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "more than 1024 columns"})
		return
	}

	schema, table := r.PathValue("schema"), r.PathValue("table")
	var cols []csvColumn
	var keep []int // append mode: header index of each kept column
	var create string
	if r.URL.Query().Get("append") == "1" {
		tcols, err := writableColumns(r.Context(), db, objName(r))
		if err != nil {
			fail(w, err)
			return
		}
		if cols, keep, err = matchColumns(header, tcols); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		if cols, err = newColumns(header); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		// Inference pass: narrow the types over all rows, holding only
		// per-column stats. Any parse error surfaces here, before DDL runs.
		for {
			row, err := cr.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				badCSV(err)
				return
			}
			observeRow(cols, row)
		}
		create = createTableSQL(schema, table, cols)
		// Rewind for the insert pass; the header parsed once already.
		if cr, err = openCSV(tmp, delim); err != nil {
			fail(w, err)
			return
		}
		if _, err := cr.Read(); err != nil {
			fail(w, err)
			return
		}
	}

	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	if create != "" {
		if _, err := tx.ExecContext(r.Context(), create); err != nil {
			tx.Rollback()
			fail(w, err)
			return
		}
	}
	insHead, per := insertHead(schema, table, cols), rowsPerInsert(cols)
	batch := make([][]string, 0, per)
	rows := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		st := insertStmt(insHead, cols, batch)
		if _, err := tx.ExecContext(r.Context(), st.SQL, st.Args...); err != nil {
			return err
		}
		rows += len(batch)
		batch = batch[:0]
		return nil
	}
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			tx.Rollback()
			badCSV(err)
			return
		}
		if keep != nil {
			row = projectRow(row, keep)
		}
		if batch = append(batch, row); len(batch) == per {
			if err := flush(); err != nil {
				tx.Rollback()
				fail(w, err)
				return
			}
		}
	}
	if err := flush(); err != nil {
		tx.Rollback()
		fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}
