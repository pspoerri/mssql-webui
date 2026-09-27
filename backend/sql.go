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
	"sync"
	"time"
	"unicode"

	"github.com/golang-sql/sqlexp"
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

// decodeBinary turns the base64 text the grid shows for binary columns back
// into bytes, in keys, set values and inserts alike. Bound as text, a binary
// key never matches, so every edit of such a table failed as "no row matched".
func decodeBinary(b batch, names, types []string) error {
	binary := map[string]bool{}
	for i, t := range types {
		binary[names[i]] = t == "BINARY" || t == "VARBINARY" || t == "IMAGE"
	}
	decode := func(m map[string]any) error {
		for c, v := range m {
			if str, ok := v.(string); ok && binary[c] {
				raw, err := base64.StdEncoding.DecodeString(str)
				if err != nil {
					return fmt.Errorf("column %q: value must be base64", c)
				}
				m[c] = raw
			}
		}
		return nil
	}
	for _, m := range b.Inserts {
		if err := decode(m); err != nil {
			return err
		}
	}
	for _, u := range b.Updates {
		if err := errors.Join(decode(u.Key), decode(u.Set)); err != nil {
			return err
		}
	}
	for _, k := range b.Deletes {
		if err := decode(k); err != nil {
			return err
		}
	}
	return nil
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

// serverEntry is one configured SQL Server: the connection template plus the
// databases to list for users who cannot open master (see listDatabases).
type serverEntry struct {
	cfg         msdsn.Config
	fallbackDBs []string
}

var (
	servers     map[string]serverEntry // by display name (host)
	serverNames []string
	errNotFound = errors.New("not found")
	// errSessionEnded: the session was dropped while a request on it ran.
	errSessionEnded = errors.New("session ended")
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
// A databases=db1|db2 parameter names the databases to show to users whose
// master login fails ('|'-separated, since ',' splits servers and Go drops
// ';' from query strings).
func parseServers(list string) (map[string]serverEntry, []string, error) {
	m := map[string]serverEntry{}
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
		e := serverEntry{cfg: cfg}
		if v := cfg.Parameters["databases"]; v != "" {
			for _, d := range strings.Split(v, "|") {
				if d = strings.TrimSpace(d); d != "" {
					e.fallbackDBs = append(e.fallbackDBs, d)
				}
			}
			delete(cfg.Parameters, "databases")
		}
		m[cfg.Host] = e
		names = append(names, cfg.Host)
	}
	if len(names) == 0 {
		return nil, nil, errors.New("no servers configured")
	}
	return m, names, nil
}

// token is the only way a SQL connection gets an access token: every connection
// opened from one of this session's pools calls it, and it answers from this
// session's token source. Two sessions never share a pool, so a connection can
// only ever carry the identity of the session that owns it.
func (s *session) token(context.Context) (string, error) {
	t, err := s.ts.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// db returns the session's pool for one database, creating it on first use.
// New connections fetch the user's current access token via token, so refresh is transparent.
func (s *session) db(ctx context.Context, srv, dbName string) (*sql.DB, error) {
	entry, ok := servers[srv]
	if !ok {
		return nil, errNotFound
	}
	cfg := entry.cfg
	key := srv + "/" + dbName
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errSessionEnded
	}
	if d, ok := s.dbs[key]; ok {
		s.mu.Unlock()
		if err := d.PingContext(ctx); err != nil {
			return nil, wrapConnect(err, dbName)
		}
		return d, nil
	}
	cfg.Database = dbName
	var conn driver.Connector
	if s.ts == nil {
		// Dev mode: user/password come from the SQL_SERVERS URL.
		conn = mssql.NewConnectorConfig(cfg)
	} else {
		c, err := mssql.NewSecurityTokenConnector(cfg, s.token)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		conn = c
	}
	// Every connect, statement and transaction on this pool is audited under the session's user.
	d := sql.OpenDB(auditConnector{Connector: conn, base: auditEvent{User: s.Email, Server: srv, DB: dbName}})
	d.SetMaxOpenConns(3)
	// ponytail: idle connections close after 5 min; the sql.DB handle and the
	// session live until logout or restart. Add a session sweeper if
	// abandoned sessions matter.
	d.SetConnMaxIdleTime(5 * time.Minute)
	s.dbs[key] = d
	s.mu.Unlock()
	if err := d.PingContext(ctx); err != nil {
		// A first connect that failed (no such database, no access, resuming)
		// keeps no pool: each one holds a goroutine, and any name in a URL
		// would otherwise add one for the session's lifetime. A canceled
		// request says nothing about the database, so its pool stays.
		if ctx.Err() == nil {
			s.mu.Lock()
			if s.dbs[key] == d {
				delete(s.dbs, key)
			}
			s.mu.Unlock()
			d.Close()
		}
		return nil, wrapConnect(err, dbName)
	}
	return d, nil
}

// Azure error numbers a paused serverless database (or a busy gateway)
// returns while it resumes; the login itself triggers the resume.
var resuming = map[int32]bool{40613: true, 40197: true, 40501: true, 49918: true, 49919: true, 49920: true}

// resumingError is a connect refused because the database is resuming. It
// is answered at once with 503 and the client retries, so the UI can say
// what it is waiting for and no request sits a minute behind a proxy timeout.
type resumingError struct {
	db  string
	err error
}

func (e *resumingError) Error() string {
	return fmt.Sprintf("database %q is paused and resuming; this can take a minute", e.db)
}
func (e *resumingError) Unwrap() error { return e.err }

// waitRetry is how long waitDB sleeps between connect attempts.
var waitRetry = 5 * time.Second

// waitDB calls open until the database has resumed, for background work
// that has no client to retry for it; onWait runs once when it starts waiting.
func waitDB(ctx context.Context, open func() (*sql.DB, error), onWait func()) (*sql.DB, error) {
	for waited := false; ; waited = true {
		d, err := open()
		var re *resumingError
		if !errors.As(err, &re) {
			return d, err
		}
		if !waited {
			onWait()
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(waitRetry):
		}
	}
}

// fail maps errors to status codes: token refresh failure means the login is
// gone (401), missing permissions are 403, SQL errors are the user's
// problem (400), the rest is ours (500).
func fail(w http.ResponseWriter, err error) {
	var re *oauth2.RetrieveError
	var ae *accessError
	var rs *resumingError
	var me mssql.Error
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.As(err, &re), errors.Is(err, errSessionEnded):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
	case errors.As(err, &ae):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": ae.Error()})
	case errors.As(err, &rs): // before mssql.Error, which it wraps
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": rs.Error(), "resuming": true})
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
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/ddl", withSession(handleDDL))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleRows))
	mux.HandleFunc("GET /api/s/{srv}/d/{db}/t/{schema}/{table}/csv", withSession(handleCSV))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/t/{schema}/{table}/csv", withSession(handleImportCSV))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/t/{schema}/{table}/rows", withSession(handleBatch))
	mux.HandleFunc("POST /api/s/{srv}/d/{db}/query", withSession(handleQuery))
	mux.HandleFunc("GET /api/jobs", withSession(handleJobs))
	mux.HandleFunc("DELETE /api/jobs/{id}", withSession(handleCancelJob))
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

// noAccess are the error numbers that mean this user cannot open the
// database, as opposed to it being paused or unreachable: login failed,
// cannot open database, cannot access it under the current security context.
var noAccess = map[int32]bool{18456: true, 4060: true, 916: true}

// accessError is a connect failure with one of the noAccess numbers: the
// database is there, but this user may not open it. The raw server text
// ("Login failed for user '<token-identified principal>'") reads like a
// broken token, so the message states what it actually means.
type accessError struct {
	db  string
	err error
}

func (e *accessError) Error() string {
	return fmt.Sprintf("you have no access to database %q (%v)", e.db, e.err)
}
func (e *accessError) Unwrap() error { return e.err }

// wrapConnect tags a no-access or resuming connect error with the database
// it was for; any other error is returned unchanged.
func wrapConnect(err error, db string) error {
	var me mssql.Error
	switch {
	case errors.As(err, &me) && noAccess[me.Number]:
		return &accessError{db: db, err: err}
	case errors.As(err, &me) && resuming[me.Number]:
		return &resumingError{db: db, err: err}
	}
	return err
}

// hasAccess reports whether the session can open srv/name. A user without a
// master login cannot ask HAS_DBACCESS, so each fallback database is probed
// with a bounded connect; unknown outcomes (paused, timeout) count as access
// so the real error surfaces when the database is opened.
func hasAccess(ctx context.Context, s *session, srv, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_, err := s.db(ctx, srv, name)
	var ae *accessError
	return !errors.As(err, &ae)
}

// listDatabases returns online databases; system=false skips master, model, msdb, tempdb.
// Enumerating needs a connection to master; a user who only exists in specific
// databases gets "Login failed for user '<token-identified principal>'" there.
// For servers with a databases=... list that failure turns into the configured
// list instead, and access errors surface when a database is opened.
func listDatabases(ctx context.Context, s *session, srv string, system bool) ([]dbInfo, error) {
	db, err := s.db(ctx, srv, "master")
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) { // expired login must stay a 401, not the fallback
			return nil, err
		}
		if fb := servers[srv].fallbackDBs; len(fb) > 0 {
			out := make([]dbInfo, len(fb))
			var wg sync.WaitGroup
			for i, name := range fb {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out[i] = dbInfo{Name: name, Access: hasAccess(ctx, s, srv, name)}
				}()
			}
			wg.Wait()
			return out, nil
		}
		var ae *accessError
		if errors.As(err, &ae) {
			return nil, fmt.Errorf("you have no access on this server: your login cannot connect to master, so databases cannot be listed (%v); if your user exists only in specific databases, add databases=... to this server's SQL_SERVERS URL", ae.err)
		}
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
	// writable gates the tree's import/create buttons. Fixed roles are listed
	// explicitly because HAS_PERMS_BY_NAME does not reflect their membership.
	var writable bool
	if err := db.QueryRowContext(r.Context(), `SELECT CAST(CASE WHEN IS_MEMBER('db_owner') = 1 OR IS_MEMBER('db_datawriter') = 1 OR IS_MEMBER('db_ddladmin') = 1
		OR HAS_PERMS_BY_NAME(NULL, NULL, 'CREATE TABLE') = 1 OR HAS_PERMS_BY_NAME(NULL, NULL, 'INSERT') = 1 THEN 1 ELSE 0 END AS BIT)`).Scan(&writable); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": schemas, "tables": tables, "writable": writable})
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

// typeLabel renders a column type the way DDL spells it: nvarchar(50), varchar(max),
// decimal(10,2), datetime2(7). User-defined types keep their own name.
func typeLabel(name, base string, maxLen, prec, scale int) string {
	if name != base && name != "" {
		return name
	}
	switch base {
	case "char", "varchar", "binary", "varbinary":
		if maxLen < 0 {
			return base + "(max)"
		}
		return fmt.Sprintf("%s(%d)", base, maxLen)
	case "nchar", "nvarchar":
		if maxLen < 0 {
			return base + "(max)"
		}
		return fmt.Sprintf("%s(%d)", base, maxLen/2)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", base, prec, scale)
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%d)", base, scale)
	}
	return base
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
		c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity, c.is_computed
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
		var maxLen, prec, scale int
		if err := rows.Scan(&c.Name, &c.Type, &base, &maxLen, &prec, &scale, &c.Nullable, &c.Identity, &computed); err != nil {
			fail(w, err)
			return
		}
		c.Type = typeLabel(c.Type, base, maxLen, prec, scale)
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
// dateTypes are rendered as text the way SQL Server prints them (style 121), so
// the grid, text search and edits all agree: 2024-01-01 00:13:00, 00:10:02, 1970-01-15.
var dateTypes = map[string]bool{"DATE": true, "TIME": true, "DATETIME": true, "DATETIME2": true, "SMALLDATETIME": true, "DATETIMEOFFSET": true}

func jsonValue(v any, dbType string) any {
	if n, ok := v.(int64); ok && dbType == "BIGINT" {
		return strconv.FormatInt(n, 10) // beyond 2^53 JavaScript numbers lose digits
	}
	if t, ok := v.(time.Time); ok {
		switch dbType {
		case "DATE":
			return t.Format("2006-01-02")
		case "TIME":
			return t.Format("15:04:05.9999999")
		case "DATETIMEOFFSET":
			return t.Format("2006-01-02 15:04:05.9999999 -07:00")
		default:
			return t.Format("2006-01-02 15:04:05.9999999")
		}
	}
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

// searchTerm is one term of a search: a bare word/phrase (op 0), or col<op>value
// with op '=' (equals), '^' (starts with) or '~' (contains).
type searchTerm struct {
	col, val string
	op       byte
}

// searchTerms splits q on whitespace; 'single' or "double" quotes keep spaces
// (and operators) inside a term, so name='User 1002' and 'two words' both work.
func searchTerms(q string) []searchTerm {
	var out []searchTerm
	var cur searchTerm
	var buf strings.Builder
	var quote rune
	has := false
	flush := func() {
		if has {
			cur.val = buf.String()
			out = append(out, cur)
		}
		cur, has = searchTerm{}, false
		buf.Reset()
	}
	for _, r := range q {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				buf.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, has = r, true
		case unicode.IsSpace(r):
			flush()
		case (r == '=' || r == '^' || r == '~') && cur.op == 0 && buf.Len() > 0:
			cur.col, cur.op = buf.String(), byte(r)
			buf.Reset()
		default:
			buf.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}

var charTypes = map[string]bool{"CHAR": true, "NCHAR": true, "VARCHAR": true, "NVARCHAR": true}

// textExpr is the column as text for LIKE, rendered the way the grid shows the
// value (see jsonValue): char columns as-is so a prefix filter can use an index,
// date/time in style 121, bit as true/false, money with four decimals.
// ponytail: floats are CAST with 6 significant digits; use col=value for exact matches.
func textExpr(col, typ string) string {
	c := quoteIdent(col)
	switch {
	case charTypes[typ]:
		return c
	case dateTypes[typ]:
		return "CONVERT(nvarchar(max), " + c + ", 121)"
	case typ == "BIT":
		return "CASE " + c + " WHEN 1 THEN 'true' WHEN 0 THEN 'false' END"
	case typ == "MONEY" || typ == "SMALLMONEY":
		return "CONVERT(nvarchar(max), " + c + ", 2)"
	}
	return "CAST(" + c + " AS nvarchar(max))"
}

func findCol(cols []string, name string) int {
	return slices.IndexFunc(cols, func(c string) bool { return strings.EqualFold(c, name) })
}

// searchWhere turns "User 1001 id=3 name^Us city~ern" into a WHERE clause: bare
// words must all occur in one searchable column (LIKE %word%); col=value is
// exact, col^value a prefix, col~value a substring; everything is ANDed.
// Placeholders start at @p1.
func searchWhere(cols, types []string, q string) (string, []any, error) {
	var conds []string
	var args []any
	var words []int // placeholder numbers of the bare words
	for _, term := range searchTerms(q) {
		if term.op == 0 {
			args = append(args, "%"+likeEscape(term.val)+"%")
			words = append(words, len(args))
			continue
		}
		i := findCol(cols, term.col)
		if i < 0 {
			return "", nil, fmt.Errorf("unknown column %q", term.col)
		}
		switch term.op {
		case '=':
			if types[i] == "BINARY" || types[i] == "VARBINARY" { // shown as base64, so compare as base64
				b, err := base64.StdEncoding.DecodeString(term.val)
				if err != nil {
					return "", nil, fmt.Errorf("column %q: value must be base64", cols[i])
				}
				args = append(args, b)
			} else {
				args = append(args, term.val)
			}
			conds = append(conds, fmt.Sprintf("%s = @p%d", quoteIdent(cols[i]), len(args)))
		default:
			if !searchableTypes[types[i]] {
				return "", nil, fmt.Errorf("column %q cannot be searched as text; use %s=value", cols[i], cols[i])
			}
			pat := likeEscape(term.val) + "%"
			if term.op == '~' {
				pat = "%" + pat
			}
			args = append(args, pat)
			conds = append(conds, fmt.Sprintf("%s LIKE @p%d ESCAPE '\\'", textExpr(cols[i], types[i]), len(args)))
		}
	}
	if len(words) > 0 {
		var ors []string
		for i, c := range cols {
			if !searchableTypes[types[i]] {
				continue
			}
			var ands []string
			for _, n := range words {
				ands = append(ands, fmt.Sprintf("%s LIKE @p%d ESCAPE '\\'", textExpr(c, types[i]), n))
			}
			ors = append(ors, "("+strings.Join(ands, " AND ")+")")
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

// orderBy builds the ORDER BY list: the sort column (validated against cols)
// first, then the primary key so paging stays stable; "(SELECT NULL)" without either.
func orderBy(cols, pk []string, sort, dir string) (string, error) {
	var parts []string
	if sort != "" {
		i := findCol(cols, sort)
		if i < 0 {
			return "", fmt.Errorf("unknown sort column %q", sort)
		}
		d := " ASC"
		if dir == "desc" {
			d = " DESC"
		}
		parts = append(parts, quoteIdent(cols[i])+d)
	}
	for _, c := range pk {
		if !strings.EqualFold(c, sort) {
			parts = append(parts, quoteIdent(c))
		}
	}
	if len(parts) == 0 {
		return "(SELECT NULL)", nil
	}
	return strings.Join(parts, ", "), nil
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
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sort := r.URL.Query().Get("sort")
	var cols, types []string
	if q != "" || sort != "" {
		if cols, types, err = columnTypes(r.Context(), db, obj); err != nil {
			fail(w, err)
			return
		}
	}
	order, err := orderBy(cols, pk, sort, r.URL.Query().Get("dir"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var where string
	var args []any
	if q != "" {
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
	writeCSV(w, rows, r.PathValue("schema")+"."+r.PathValue("table")+".csv")
}

// writeCSV streams the current result set of rows as a CSV download. Past
// the headers an error aborts the connection, so the browser reports a
// failed download instead of saving a truncated file.
func writeCSV(w http.ResponseWriter, rows *sql.Rows, filename string) {
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	cw := csv.NewWriter(w)
	cw.Write(cols)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	rec := make([]string, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	abort := func(err error) {
		log.Printf("csv %s: %v", filename, err)
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
	names, types, err := columnTypes(r.Context(), db, objName(r))
	if err != nil {
		fail(w, err)
		return
	}
	if err := decodeBinary(b, names, types); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
// maxConsoleRows caps each result set the console returns; the rest is read
// and dropped so the statements after it still run.
const maxConsoleRows = 1000

// handleQuery runs a console batch and reports everything it produced, in
// order: result sets, rows-affected counts and PRINT/info messages, like
// SSMS. A SQL error answers 400 but keeps the results before it. With
// ?format=csv&set=N the Nth result set (from 0) is streamed as a CSV
// download instead, all of its rows.
// The batch runs on its own connection so a transaction it leaves open can
// be rolled back here, rather than holding locks until the pool reuses it.
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
	ctx := r.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer conn.Close()
	// A transaction the batch leaves open (no COMMIT, or a CSV download that
	// stopped the batch after its result set) is rolled back here, on every
	// path; otherwise it would go back to the pool holding its locks.
	var leftOpenCount int
	defer func() {
		ctx := context.WithoutCancel(ctx)
		var open int
		if conn.QueryRowContext(ctx, "SELECT @@TRANCOUNT").Scan(&open) == nil && open > 0 {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	msg := &sqlexp.ReturnMessage{}
	rows, err := conn.QueryContext(ctx, body.SQL, msg)
	if err != nil {
		fail(w, err)
		return
	}
	asCSV := r.URL.Query().Get("format") == "csv"
	set, _ := strconv.Atoi(r.URL.Query().Get("set"))
	results, messages, errs := []map[string]any{}, []string{}, []string{}
	afterSet := false // a SELECT's rows-affected count follows its result set; that one is not shown
	for active := true; active; {
		switch m := msg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			messages = append(messages, m.Message.String())
		case sqlexp.MsgNext:
			if asCSV {
				if set == 0 {
					writeCSV(w, rows, "query.csv")
					rows.Close()
					return
				}
				set--
				for rows.Next() {
				}
				continue
			}
			cols, data, err := readRows(rows, maxConsoleRows)
			truncated := false
			if len(data) == maxConsoleRows { // drain the rest; Next again after it said false loses what follows
				for rows.Next() {
					truncated = true
				}
			}
			if err != nil {
				errs = append(errs, err.Error())
			}
			results = append(results, map[string]any{"columns": cols, "rows": data, "truncated": truncated})
			afterSet = true
		case sqlexp.MsgRowsAffected:
			if !afterSet {
				results = append(results, map[string]any{"rowsAffected": m.Count})
			}
			afterSet = false
		case sqlexp.MsgError:
			var me mssql.Error
			if errors.As(m.Error, &me) {
				errs = append(errs, sqlMessages(me))
			} else {
				errs = append(errs, m.Error.Error())
			}
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		}
	}
	if err := rows.Close(); err != nil && len(errs) == 0 {
		errs = append(errs, err.Error())
	}
	if conn.QueryRowContext(ctx, "SELECT @@TRANCOUNT").Scan(&leftOpenCount) == nil && leftOpenCount > 0 {
		errs = append(errs, "the batch left a transaction open, so it was rolled back; put COMMIT (or ROLLBACK) in the same batch")
	}
	if asCSV {
		msg := "the query returned no such result set to download"
		if len(errs) > 0 {
			msg = strings.Join(errs, "\n")
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	out := map[string]any{"results": results, "messages": messages}
	if len(errs) > 0 {
		out["error"] = strings.Join(errs, "\n")
		writeJSON(w, http.StatusBadRequest, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
