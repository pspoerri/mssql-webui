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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// CSV import: POST a file to .../t/{schema}/{table}/csv and a background job
// creates the table from it. The header row names the columns, and every
// column gets the narrowest type that fits all of its values; values are
// bulk-copied typed, in one transaction with the CREATE TABLE, so a failed
// import leaves nothing behind.

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
	// Codes with a leading zero (zip, phone, account numbers) stay text; as
	// numbers they would lose the zero. "0" and "0.5" are still numbers.
	if d := strings.TrimLeft(v, "+-"); len(d) > 1 && d[0] == '0' && d[1] >= '0' && d[1] <= '9' {
		return k | dateKinds(v)
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
	return k | dateKinds(v)
}

func dateKinds(v string) int {
	k := 0
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

// maxImportBytes caps an upload. The body is spooled to disk, not memory,
// so the cap only protects the temp filesystem.
const maxImportBytes = 2 << 30

// openCSV starts reading the spooled file from the top: past Excel's UTF-8
// BOM, with the sniffed delimiter. read counts the bytes consumed, for progress.
func openCSV(f *os.File, delim rune, read *atomic.Int64) (*csv.Reader, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	read.Store(0)
	br := bufio.NewReaderSize(countReader{f, read}, 64<<10)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\ufeff")) {
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.Comma = delim
	cr.ReuseRecord = true
	return cr, nil
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// importJob is one CSV import running in the background. The upload request
// returns once the file is spooled and the UI polls GET /api/jobs: a file of
// millions of rows takes minutes, longer than proxies let a request live,
// and a dropped request would have rolled the whole import back.
// ponytail: jobs live in the session, so they end with it (logout, 12 h, or
// 30 min without requests once the tab stops polling). Persist them if
// imports must outlive the browser.
type importJob struct {
	ID, Srv, DB, Schema, Table string
	Size                       int64
	appendRows                 bool
	read, rows                 atomic.Int64 // progress, updated per row without the lock
	cancel                     context.CancelFunc
	mu                         sync.Mutex
	state, phase, err          string // state: running, done, failed, canceled
	started, ended             time.Time
}

func (j *importJob) set(phase string) {
	j.mu.Lock()
	j.phase = phase
	j.mu.Unlock()
}

func (j *importJob) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"id": j.ID, "kind": "import", "srv": j.Srv, "db": j.DB, "schema": j.Schema, "table": j.Table,
		"append": j.appendRows, "state": j.state, "phase": j.phase, "error": j.err,
		"bytes": j.read.Load(), "size": j.Size, "rows": j.rows.Load(), "started": j.started}
}

// jobError is the message a failed job shows; the same texts fail() would send.
func jobError(err error) string {
	var me mssql.Error
	switch {
	case errors.Is(err, errNotFound):
		return "table not found"
	case errors.As(err, &me):
		return sqlMessages(me)
	}
	return err.Error()
}

// handleImportCSV spools the posted CSV to a temp file and starts an import
// job for [schema].[table]: without append=1 the table is created, its
// column types inferred from the values; with append=1 the rows go into the
// existing table, converted by SQL Server like grid edits. It answers 202
// with the job; everything else, errors included, is reported by the job.
func handleImportCSV(w http.ResponseWriter, r *http.Request, s *session) {
	if _, ok := servers[r.PathValue("srv")]; !ok {
		fail(w, errNotFound)
		return
	}
	tmp, err := os.CreateTemp("", "mssql-webui-import-*.csv")
	if err != nil {
		fail(w, err)
		return
	}
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &importJob{ID: randomID()[:16], Srv: r.PathValue("srv"), DB: r.PathValue("db"), Schema: r.PathValue("schema"),
		Table: r.PathValue("table"), Size: n, appendRows: r.URL.Query().Get("append") == "1", cancel: cancel,
		state: "running", started: time.Now()}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*importJob{}
	}
	s.jobs[j.ID] = j
	s.mu.Unlock()
	go j.run(ctx, s, tmp)
	writeJSON(w, http.StatusAccepted, j.view())
}

func (j *importJob) run(ctx context.Context, s *session, f *os.File) {
	defer os.Remove(f.Name())
	defer f.Close()
	err := j.load(ctx, s, f)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ended, j.phase = time.Now(), ""
	switch {
	case err == nil:
		j.state = "done"
	case ctx.Err() != nil:
		j.state, j.err = "canceled", "canceled"
	default:
		j.state, j.err = "failed", jobError(err)
	}
	j.cancel()
}

// load imports the spooled file in one transaction: a failed or canceled
// import leaves nothing behind (a new table is dropped again). Rows go in by bulk copy, not INSERTs, which
// is what makes millions of rows feasible. A new table is filled directly
// with the inferred Go values; an append goes through a #stage table of
// nvarchar columns and INSERT ... SELECT, so SQL Server converts the text and
// the table's triggers, constraints and defaults apply as for any insert.
func (j *importJob) load(ctx context.Context, s *session, f *os.File) (err error) {
	j.set("connecting")
	db, err := waitDB(ctx, func() (*sql.DB, error) { return s.db(ctx, j.Srv, j.DB) },
		func() { j.set("waiting for the database to resume") })
	if err != nil {
		return err
	}
	head := make([]byte, 64<<10)
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return err
	}
	delim := sniffDelim(bytes.TrimPrefix(head[:n], []byte("\ufeff")))
	cr, err := openCSV(f, delim, &j.read)
	if err != nil {
		return err
	}
	header, err := cr.Read()
	if err == io.EOF {
		return errors.New("empty file; the first row must name the columns")
	}
	if err != nil {
		return fmt.Errorf("bad csv: %w", err)
	}
	header = slices.Clone(header) // ReuseRecord: the next Read overwrites it
	if len(header) > 1024 {
		return errors.New("more than 1024 columns")
	}

	obj := quoteIdent(j.Schema) + "." + quoteIdent(j.Table)
	var cols []csvColumn
	var keep []int // append: header index of each kept column
	if j.appendRows {
		tcols, err := writableColumns(ctx, db, obj)
		if err != nil {
			return err
		}
		if cols, keep, err = matchColumns(header, tcols); err != nil {
			return err
		}
	} else {
		if cols, err = newColumns(header); err != nil {
			return err
		}
		// Inference pass: narrow the types over all rows, holding only
		// per-column stats. Any parse error surfaces here, before DDL runs.
		j.set("checking types")
		for {
			row, err := cr.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("bad csv: %w", err)
			}
			observeRow(cols, row)
		}
		if cr, err = openCSV(f, delim, &j.read); err != nil {
			return err
		}
		if _, err := cr.Read(); err != nil {
			return err
		}
	}

	dest, names := obj, make([]string, len(cols))
	if !j.appendRows {
		// The CREATE commits on its own: an uncommitted new table blocks every
		// catalog read of the database (the tree, for all users) until the
		// import ends. A failed import drops it again.
		// ponytail: a crash mid-import leaves the empty table behind.
		if _, err := db.ExecContext(ctx, createTableSQL(j.Schema, j.Table, cols)); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				db.ExecContext(context.Background(), "DROP TABLE "+obj)
			}
		}()
		for i, c := range cols {
			names[i] = c.Name
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed
	if j.appendRows {
		defs := make([]string, len(cols))
		for i := range cols {
			names[i] = fmt.Sprintf("c%d", i)
			defs[i] = names[i] + " nvarchar(max) NULL"
		}
		dest = "#stage"
		if _, err = tx.ExecContext(ctx, "CREATE TABLE #stage ("+strings.Join(defs, ", ")+")"); err != nil {
			return err
		}
	}
	j.set("inserting")
	bulk, err := tx.PrepareContext(ctx, mssql.CopyIn(dest, mssql.BulkOptions{Tablock: true}, names...))
	if err != nil {
		return err
	}
	defer bulk.Close()
	vals := make([]any, len(cols))
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("bad csv: %w", err)
		}
		if keep != nil {
			row = projectRow(row, keep)
		}
		for i, c := range cols {
			vals[i] = c.sqlValue(row[i])
		}
		if _, err := bulk.ExecContext(ctx, vals...); err != nil {
			return err
		}
		j.rows.Add(1)
	}
	if _, err := bulk.ExecContext(ctx); err != nil { // flush
		return err
	}
	if j.appendRows {
		j.set("copying into the table")
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = quoteIdent(c.Name)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+obj+" ("+strings.Join(quoted, ", ")+") SELECT "+strings.Join(names, ", ")+" FROM #stage; DROP TABLE #stage"); err != nil {
			return err
		}
	}
	j.set("committing")
	return tx.Commit()
}

// handleJobs lists the session's imports, dropping those finished over an hour ago.
func handleJobs(w http.ResponseWriter, r *http.Request, s *session) {
	var jobs []*importJob
	s.mu.Lock()
	for id, j := range s.jobs {
		j.mu.Lock()
		old := !j.ended.IsZero() && time.Since(j.ended) > time.Hour
		j.mu.Unlock()
		if old {
			delete(s.jobs, id)
		} else {
			jobs = append(jobs, j)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(jobs, func(a, b *importJob) int { return a.started.Compare(b.started) })
	out := make([]map[string]any, len(jobs))
	for i, j := range jobs {
		out[i] = j.view()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCancelJob cancels a running import (its transaction rolls back) or
// forgets a finished one.
func handleCancelJob(w http.ResponseWriter, r *http.Request, s *session) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j != nil {
		j.mu.Lock()
		if j.state != "running" {
			delete(s.jobs, j.ID)
		}
		j.mu.Unlock()
	}
	s.mu.Unlock()
	if j == nil {
		fail(w, errNotFound)
		return
	}
	j.cancel()
	w.WriteHeader(http.StatusNoContent)
}
