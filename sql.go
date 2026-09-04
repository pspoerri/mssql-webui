package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
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
			out = append(out, stmt{SQL: "INSERT INTO " + t + " DEFAULT VALUES"})
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
		out = append(out, stmt{SQL: "UPDATE " + t + " SET " + strings.Join(sets, ", ") + " WHERE " + where, Args: args})
	}
	for _, key := range b.Deletes {
		if len(key) == 0 {
			return nil, errors.New("delete without key")
		}
		where, args := whereClause(key, nil)
		out = append(out, stmt{SQL: "DELETE FROM " + t + " WHERE " + where, Args: args})
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
func (s *session) db(srv, dbName string) (*sql.DB, error) {
	cfg, ok := servers[srv]
	if !ok {
		return nil, errNotFound
	}
	key := srv + "/" + dbName
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dbs[key]; ok {
		return d, nil
	}
	cfg.Database = dbName
	conn, err := mssql.NewSecurityTokenConnector(cfg, func(ctx context.Context) (string, error) {
		t, err := s.ts.Token()
		if err != nil {
			return "", err
		}
		return t.AccessToken, nil
	})
	if err != nil {
		return nil, err
	}
	d := sql.OpenDB(conn)
	d.SetMaxOpenConns(3)
	d.SetConnMaxIdleTime(5 * time.Minute)
	s.dbs[key] = d
	return d, nil
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": me.Message})
	default:
		log.Printf("error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers", withSession(handleServers))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/tables", withSession(handleTables))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}", withSession(handleColumns))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleRows))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleBatch))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/query", withSession(handleQuery))
}

type serverInfo struct {
	Name      string   `json:"name"`
	Databases []string `json:"databases"`
	Error     string   `json:"error,omitempty"`
}

func listDatabases(ctx context.Context, s *session, srv string) ([]string, error) {
	db, err := s.db(srv, "master")
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sys.databases WHERE state = 0 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func handleServers(w http.ResponseWriter, r *http.Request, s *session) {
	out := []serverInfo{}
	for _, name := range serverNames {
		info := serverInfo{Name: name, Databases: []string{}}
		dbs, err := listDatabases(r.Context(), s, name)
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

func handleTables(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := db.QueryContext(r.Context(), `SELECT s.name, o.name, o.type
		FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id
		WHERE o.type IN ('U', 'V') ORDER BY s.name, o.name`)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	out := []tableInfo{}
	for rows.Next() {
		var t tableInfo
		var typ string
		if err := rows.Scan(&t.Schema, &t.Name, &typ); err != nil {
			fail(w, err)
			return
		}
		t.Kind = "table"
		if strings.TrimSpace(typ) == "V" {
			t.Kind = "view"
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type columnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Identity bool   `json:"identity"`
	Readonly bool   `json:"readonly"`
}

// Base types the grid never writes: binary blobs, rowversion, CLR types.
var readonlyTypes = map[string]bool{
	"binary": true, "varbinary": true, "image": true, "timestamp": true,
	"hierarchyid": true, "sql_variant": true,
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
	db, err := s.db(r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	obj := objName(r)
	rows, err := db.QueryContext(r.Context(), `SELECT c.name, TYPE_NAME(c.user_type_id), TYPE_NAME(c.system_type_id),
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

func handleRows(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.PathValue("srv"), r.PathValue("db"))
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
	if limit <= 0 || limit > 500 {
		limit = 100
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
	rows, err := db.QueryContext(r.Context(),
		fmt.Sprintf("SELECT * FROM %s ORDER BY %s OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY", obj, order),
		offset, limit+1)
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

// handleBatch applies inserts/updates/deletes in one transaction.
// ponytail: values are strings and SQL Server converts them; last write wins.
// Add typed conversion or a rowversion check if either bites.
func handleBatch(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.PathValue("srv"), r.PathValue("db"))
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
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleQuery runs ad-hoc SQL.
// ponytail: SELECT/WITH return the first result set, anything else returns
// rows affected. Multiple result sets are dropped; add NextResultSet if needed.
func handleQuery(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.PathValue("srv"), r.PathValue("db"))
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
