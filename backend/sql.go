package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
	"golang.org/x/oauth2"
)

// quoteIdent brackets a SQL Server identifier. Every identifier that reaches
// SQL text goes through here.
func quoteIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}

type update struct {
	Key map[string]any `json:"key"`
	Set map[string]any `json:"set"`
}

type batch struct {
	Inserts []map[string]any `json:"inserts"`
	Updates []update         `json:"updates"`
	Deletes []map[string]any `json:"deletes"`
}

type stmt struct {
	SQL  string
	Args []any
	Kind string // "insert", "update", or "delete"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// whereClause appends "[c] = @pN" conditions for each key column to args.
func whereClause(key map[string]any, args []any) (string, []any) {
	var conds []string
	for _, c := range sortedKeys(key) {
		args = append(args, key[c])
		conds = append(conds, fmt.Sprintf("%s = @p%d", quoteIdent(c), len(args)))
	}
	return strings.Join(conds, " AND "), args
}

// buildBatch turns an edit batch into parameterized statements against
// [schema].[table]. Updates and deletes without a key are refused so a bad
// client can never touch every row.
func buildBatch(schema, table string, b batch) ([]stmt, error) {
	t := quoteIdent(schema) + "." + quoteIdent(table)
	var out []stmt
	for _, row := range b.Inserts {
		cols := sortedKeys(row)
		if len(cols) == 0 {
			out = append(out, stmt{SQL: "INSERT INTO " + t + " DEFAULT VALUES", Kind: "insert"})
			continue
		}
		var names, marks []string
		var args []any
		for _, c := range cols {
			args = append(args, row[c])
			names = append(names, quoteIdent(c))
			marks = append(marks, fmt.Sprintf("@p%d", len(args)))
		}
		out = append(out, stmt{
			SQL:  "INSERT INTO " + t + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")",
			Args: args,
			Kind: "insert",
		})
	}
	for _, u := range b.Updates {
		if len(u.Key) == 0 {
			return nil, errors.New("update without key")
		}
		if len(u.Set) == 0 {
			continue
		}
		var sets []string
		var args []any
		for _, c := range sortedKeys(u.Set) {
			args = append(args, u.Set[c])
			sets = append(sets, fmt.Sprintf("%s = @p%d", quoteIdent(c), len(args)))
		}
		where, args := whereClause(u.Key, args)
		out = append(out, stmt{SQL: "UPDATE " + t + " SET " + strings.Join(sets, ", ") + " WHERE " + where, Args: args, Kind: "update"})
	}
	for _, key := range b.Deletes {
		if len(key) == 0 {
			return nil, errors.New("delete without key")
		}
		where, args := whereClause(key, nil)
		out = append(out, stmt{SQL: "DELETE FROM " + t + " WHERE " + where, Args: args, Kind: "delete"})
	}
	return out, nil
}

var (
	servers     map[string]msdsn.Config // by display name (host)
	serverNames []string
	errNotFound = errors.New("not found")
)

func initSQL() {
	var err error
	servers, serverNames, err = parseServers(mustEnv("SQL_SERVERS"))
	if err != nil {
		log.Fatalf("SQL_SERVERS: %v", err)
	}
}

// parseServers reads comma-separated go-mssqldb URLs without credentials,
// e.g. "sqlserver://sql1.internal:1433?encrypt=true,sqlserver://sql2?trustservercertificate=true".
func parseServers(list string) (map[string]msdsn.Config, []string, error) {
	m := map[string]msdsn.Config{}
	var names []string
	for _, u := range strings.Split(list, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		cfg, err := msdsn.Parse(u)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", u, err)
		}
		if _, ok := m[cfg.Host]; ok {
			return nil, nil, fmt.Errorf("duplicate server host %q", cfg.Host)
		}
		m[cfg.Host] = cfg
		names = append(names, cfg.Host)
	}
	if len(names) == 0 {
		return nil, nil, errors.New("no servers configured")
	}
	return m, names, nil
}

// db returns the session's pool for one database, creating it on first use.
// New connections fetch the user's current access token, so refresh is transparent.
func (s *session) db(ctx context.Context, srv, dbName string) (*sql.DB, error) {
	cfg, ok := servers[srv]
	if !ok {
		return nil, errNotFound
	}
	key := srv + "/" + dbName
	s.mu.Lock()
	if d, ok := s.dbs[key]; ok {
		s.mu.Unlock()
		if err := wake(ctx, d); err != nil {
			return nil, err
		}
		return d, nil
	}
	cfg.Database = dbName
	var conn driver.Connector
	if s.ts == nil {
		// Dev mode: user/password come from the SQL_SERVERS URL.
		conn = mssql.NewConnectorConfig(cfg)
	} else {
		c, err := mssql.NewSecurityTokenConnector(cfg, func(ctx context.Context) (string, error) {
			t, err := s.ts.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		})
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		conn = c
	}
	d := sql.OpenDB(conn)
	d.SetMaxOpenConns(3)
	// ponytail: idle connections close after 5 min; the sql.DB handle and the
	// session live until logout or restart. Add a session sweeper if
	// abandoned sessions matter.
	d.SetConnMaxIdleTime(5 * time.Minute)
	s.dbs[key] = d
	s.mu.Unlock()
	if err := wake(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// Azure error numbers a paused serverless database (or a busy gateway)
// returns while it resumes; the login itself triggers the resume.
var resuming = map[int32]bool{40613: true, 40197: true, 40501: true, 49918: true, 49919: true, 49920: true}

var wakeRetry = 5 * time.Second

// wake pings d and retries while the server says the database is resuming.
// ponytail: one ping per request; cache a last-seen-alive time per handle if
// the extra round trip shows up.
func wake(ctx context.Context, d *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := d.PingContext(ctx)
		var me mssql.Error
		if err == nil || !errors.As(err, &me) || !resuming[me.Number] {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wakeRetry):
		}
	}
}

// fail maps errors to status codes: token refresh failure means the login is
// gone (401), SQL errors are the user's problem (400), the rest is ours (500).
func fail(w http.ResponseWriter, err error) {
	var re *oauth2.RetrieveError
	var me mssql.Error
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.As(err, &re):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
	case errors.As(err, &me):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": sqlMessages(me)})
	default:
		log.Printf("error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// sqlMessages joins every message the server sent, so e.g. CREATE DATABASE's
// "check related errors" comes with the related error.
func sqlMessages(me mssql.Error) string {
	if len(me.All) < 2 {
		return me.Message
	}
	msgs := make([]string, len(me.All))
	for i, e := range me.All {
		msgs[i] = e.Message
	}
	return strings.Join(msgs, "\n")
}

func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers", withSession(handleServers))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/tables", withSession(handleTables))
	mux.HandleFunc("POST /api/s/{srv}/databases", withSession(handleCreateDatabase))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/schemas", withSession(handleCreateSchema))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}", withSession(handleColumns))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleRows))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/csv", withSession(handleCSV))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleBatch))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/query", withSession(handleQuery))
}

type serverInfo struct {
	Name      string   `json:"name"`
	Databases []dbInfo `json:"databases"`
	Error     string   `json:"error,omitempty"`
}

type dbInfo struct {
	Name   string `json:"name"`
	Access bool   `json:"access"` // HAS_DBACCESS; only an explicit 0 is reported as no access
}

// listDatabases returns online databases; system=false skips master, model, msdb, tempdb.
func listDatabases(ctx context.Context, s *session, srv string, system bool) ([]dbInfo, error) {
	db, err := s.db(ctx, srv, "master")
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name, HAS_DBACCESS(name) FROM sys.databases
		WHERE state = 0 AND (database_id > 4 OR @p1 = 1) ORDER BY name`, system)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dbInfo{}
	for rows.Next() {
		var d dbInfo
		var access sql.NullInt64
		if err := rows.Scan(&d.Name, &access); err != nil {
			return nil, err
		}
		d.Access = !access.Valid || access.Int64 != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

func handleServers(w http.ResponseWriter, r *http.Request, s *session) {
	out := []serverInfo{}
	for _, name := range serverNames {
		info := serverInfo{Name: name, Databases: []dbInfo{}}
		dbs, err := listDatabases(r.Context(), s, name, r.URL.Query().Get("system") == "1")
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			fail(w, err)
			return
		}
		if err != nil {
			info.Error = err.Error()
		} else {
			info.Databases = dbs
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

type tableInfo struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// handleTables lists user schemas and their tables and views. Empty schemas are
// included so a freshly created one shows up in the tree.
func handleTables(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := db.QueryContext(r.Context(), `SELECT s.name, o.name, o.type
		FROM sys.schemas s LEFT JOIN sys.objects o ON o.schema_id = s.schema_id AND o.type IN ('U', 'V')
		WHERE s.schema_id < 16384 AND s.name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest')
		ORDER BY s.name, o.name`)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	schemas, tables := []string{}, []tableInfo{}
	for rows.Next() {
		var schema string
		var name, typ sql.NullString
		if err := rows.Scan(&schema, &name, &typ); err != nil {
			fail(w, err)
			return
		}
		if len(schemas) == 0 || schemas[len(schemas)-1] != schema {
			schemas = append(schemas, schema)
		}
		if !name.Valid {
			continue
		}
		t := tableInfo{Schema: schema, Name: name.String, Kind: "table"}
		if strings.TrimSpace(typ.String) == "V" {
			t.Kind = "view"
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": schemas, "tables": tables})
}

// createNamed runs `ddl [name]` with the name from the JSON body {"name": ...}.
func createNamed(w http.ResponseWriter, r *http.Request, s *session, dbName, ddl string) {
	db, err := s.db(r.Context(), r.PathValue("srv"), dbName)
	if err != nil {
		fail(w, err)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if _, err := db.ExecContext(r.Context(), ddl+" "+quoteIdent(body.Name)); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": body.Name})
}

func handleCreateSchema(w http.ResponseWriter, r *http.Request, s *session) {
	createNamed(w, r, s, r.PathValue("db"), "CREATE SCHEMA")
}

func handleCreateDatabase(w http.ResponseWriter, r *http.Request, s *session) {
	createNamed(w, r, s, "master", "CREATE DATABASE")
}

type columnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Identity bool   `json:"identity"`
	Readonly bool   `json:"readonly"`
}

// Base type names the grid never writes: binary blobs, rowversion, CLR
// types. CLR types (hierarchyid, geometry, geography, user CLR types) have
// no system_type_id row in sys.types, so the query falls back to their user
// type name for this check.
var readonlyTypes = map[string]bool{
	"binary": true, "varbinary": true, "image": true, "timestamp": true,
	"hierarchyid": true, "sql_variant": true, "geometry": true, "geography": true,
}

func primaryKey(ctx context.Context, db *sql.DB, obj string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.name FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = OBJECT_ID(@p1) AND i.is_primary_key = 1 ORDER BY ic.key_ordinal`, obj)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pk := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		pk = append(pk, n)
	}
	return pk, rows.Err()
}

func objName(r *http.Request) string {
	return quoteIdent(r.PathValue("schema")) + "." + quoteIdent(r.PathValue("table"))
}

func handleColumns(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	obj := objName(r)
	rows, err := db.QueryContext(r.Context(), `SELECT c.name, COALESCE(TYPE_NAME(c.user_type_id), ''),
		COALESCE(TYPE_NAME(c.system_type_id), TYPE_NAME(c.user_type_id), ''),
		c.is_nullable, c.is_identity, c.is_computed
		FROM sys.columns c WHERE c.object_id = OBJECT_ID(@p1) ORDER BY c.column_id`, obj)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	cols := []columnInfo{}
	for rows.Next() {
		var c columnInfo
		var base string
		var computed bool
		if err := rows.Scan(&c.Name, &c.Type, &base, &c.Nullable, &c.Identity, &computed); err != nil {
			fail(w, err)
			return
		}
		c.Readonly = c.Identity || computed || readonlyTypes[base]
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	if len(cols) == 0 {
		fail(w, errNotFound)
		return
	}
	pk, err := primaryKey(r.Context(), db, obj)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "pk": pk})
}

// jsonValue makes driver values JSON-safe. go-mssqldb hands back decimals and
// money as []byte digit strings, GUIDs as 16 raw bytes, binary as bytes.
func jsonValue(v any, dbType string) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	switch dbType {
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		return string(b)
	case "UNIQUEIDENTIFIER":
		var u mssql.UniqueIdentifier
		if u.Scan(b) == nil {
			return u.String()
		}
	}
	return base64.StdEncoding.EncodeToString(b)
}

// readRows drains up to max rows into JSON-friendly values.
func readRows(rows *sql.Rows, max int) ([]string, [][]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for len(out) < max && rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			vals[i] = jsonValue(v, types[i].DatabaseTypeName())
		}
		out = append(out, vals)
	}
	return cols, out, rows.Err()
}

// Column types free-text search can CAST to nvarchar; anything else (binary,
// CLR, sql_variant) is only reachable through col=value.
var searchableTypes = map[string]bool{
	"CHAR": true, "NCHAR": true, "VARCHAR": true, "NVARCHAR": true, "TEXT": true, "NTEXT": true, "XML": true,
	"TINYINT": true, "SMALLINT": true, "INT": true, "BIGINT": true, "DECIMAL": true, "MONEY": true, "SMALLMONEY": true,
	"FLOAT": true, "REAL": true, "BIT": true, "UNIQUEIDENTIFIER": true,
	"DATE": true, "DATETIME": true, "DATETIME2": true, "SMALLDATETIME": true, "DATETIMEOFFSET": true, "TIME": true,
}

// searchWhere turns "alan id=3" into a WHERE clause: a bare word matches any
// searchable column (LIKE %word%), col=value matches that column exactly;
// terms are ANDed. Placeholders start at @p1.
// ponytail: terms split on whitespace, so a value cannot contain spaces; add quoting if needed.
func searchWhere(cols, types []string, q string) (string, []any, error) {
	var conds []string
	var args []any
	for _, term := range strings.Fields(q) {
		if col, val, ok := strings.Cut(term, "="); ok && col != "" {
			i := slices.IndexFunc(cols, func(c string) bool { return strings.EqualFold(c, col) })
			if i < 0 {
				return "", nil, fmt.Errorf("unknown column %q", col)
			}
			args = append(args, val)
			conds = append(conds, fmt.Sprintf("%s = @p%d", quoteIdent(cols[i]), len(args)))
			continue
		}
		var ors []string
		args = append(args, "%"+likeEscape(term)+"%")
		for i, c := range cols {
			if searchableTypes[types[i]] {
				ors = append(ors, fmt.Sprintf("CAST(%s AS nvarchar(max)) LIKE @p%d ESCAPE '\\'", quoteIdent(c), len(args)))
			}
		}
		if len(ors) == 0 {
			return "", nil, errors.New("no searchable columns; use col=value")
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`, "[", `\[`).Replace(s)
}

// columnTypes returns the column names and driver type names of obj without reading rows.
func columnTypes(ctx context.Context, db *sql.DB, obj string) ([]string, []string, error) {
	rows, err := db.QueryContext(ctx, "SELECT TOP 0 * FROM "+obj)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, len(types))
	tn := make([]string, len(types))
	for i, t := range types {
		names[i], tn[i] = t.Name(), t.DatabaseTypeName()
	}
	return names, tn, nil
}

func handleRows(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	obj := objName(r)
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	pk, err := primaryKey(r.Context(), db, obj)
	if err != nil {
		fail(w, err)
		return
	}
	order := "(SELECT NULL)"
	if len(pk) > 0 {
		var q []string
		for _, c := range pk {
			q = append(q, quoteIdent(c))
		}
		order = strings.Join(q, ", ")
	}
	var where string
	var args []any
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		cols, types, err := columnTypes(r.Context(), db, obj)
		if err != nil {
			fail(w, err)
			return
		}
		if where, args, err = searchWhere(cols, types, q); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	args = append(args, offset, limit+1)
	rows, err := db.QueryContext(r.Context(),
		fmt.Sprintf("SELECT * FROM %s%s ORDER BY %s OFFSET @p%d ROWS FETCH NEXT @p%d ROWS ONLY", obj, where, order, len(args)-1, len(args)),
		args...)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	cols, data, err := readRows(rows, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	hasMore := len(data) > limit
	if hasMore {
		data = data[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": data, "hasMore": hasMore})
}

// handleCSV streams the whole table or view as a CSV download.
// ponytail: NULL becomes an empty field; add a quoting convention if someone needs to tell "" and NULL apart.
func handleCSV(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := db.QueryContext(r.Context(), "SELECT * FROM "+objName(r))
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		fail(w, err)
		return
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", r.PathValue("schema")+"."+r.PathValue("table")+".csv"))
	cw := csv.NewWriter(w)
	cw.Write(cols)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	rec := make([]string, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	abort := func(err error) {
		// Headers are already out; abort the connection so the browser reports a failed download instead of a truncated file.
		log.Printf("csv %s: %v", objName(r), err)
		panic(http.ErrAbortHandler)
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			abort(err)
		}
		for i, v := range vals {
			rec[i] = csvString(jsonValue(v, types[i].DatabaseTypeName()))
		}
		cw.Write(rec)
	}
	if err := rows.Err(); err != nil {
		abort(err)
	}
	cw.Flush()
}

func csvString(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// handleBatch applies inserts/updates/deletes in one transaction.
// ponytail: values are strings and SQL Server converts them; last write wins.
// Add typed conversion or a rowversion check if either bites.
func handleBatch(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	var b batch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	stmts, err := buildBatch(r.PathValue("schema"), r.PathValue("table"), b)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	for _, st := range stmts {
		res, err := tx.ExecContext(r.Context(), st.SQL, st.Args...)
		if err != nil {
			tx.Rollback()
			fail(w, err)
			return
		}
		if st.Kind == "update" || st.Kind == "delete" {
			if n, err := res.RowsAffected(); err == nil && n == 0 {
				tx.Rollback()
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no row matched the key; it may have been changed or deleted"})
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleQuery runs ad-hoc SQL.
// ponytail: SELECT/WITH return the first result set, anything else returns
// rows affected. Multiple result sets are dropped; add NextResultSet if needed.
func handleQuery(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	q := strings.ToUpper(strings.TrimSpace(body.SQL))
	if strings.HasPrefix(q, "SELECT") || strings.HasPrefix(q, "WITH") {
		rows, err := db.QueryContext(r.Context(), body.SQL)
		if err != nil {
			fail(w, err)
			return
		}
		defer rows.Close()
		cols, data, err := readRows(rows, 1000)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": data})
		return
	}
	res, err := db.ExecContext(r.Context(), body.SQL)
	if err != nil {
		fail(w, err)
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]int64{"rowsAffected": n})
}
