package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// The audit log: one JSON object per line on stdout, kept apart from the
// diagnostics the log package writes to stderr, so a collector can take
// stdout as the audit stream. auth.go writes logins and logouts. Every SQL
// statement is caught here, at the driver: session.db wraps each pool's
// connector in auditConnector, database/sql prepares every statement through
// the wrapped connection, and the wrapped statement and result set record the
// outcome. No handler can reach SQL Server around this.

var auditOut = log.New(os.Stdout, "", 0)

// maxAuditSQL caps the statement text in one line; container log drivers
// split lines beyond 16 KB. A longer statement is written in full over
// several lines ("part" 1..n of "parts", sharing an "id"), never cut: a cut
// line could hide a DROP behind padding.
const maxAuditSQL = 8 << 10

type auditEvent struct {
	Time   string  `json:"time"`
	Event  string  `json:"event"` // login, logout, connect, query, exec, begin, commit, rollback
	User   string  `json:"user,omitempty"`
	Name   string  `json:"name,omitempty"`
	Server string  `json:"server,omitempty"`
	DB     string  `json:"db,omitempty"`
	Reason string  `json:"reason,omitempty"` // logout: user, idle, expired, relogin
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Ms     float64 `json:"ms,omitempty"`
	Rows   *int64  `json:"rows,omitempty"`  // exec: rows affected; query: rows read
	ID     string  `json:"id,omitempty"`    // a statement split over several lines
	Part   int     `json:"part,omitempty"`  // 1..Parts
	Parts  int     `json:"parts,omitempty"` // number of lines of the statement
	SQL    string  `json:"sql,omitempty"`
}

// audit writes e with the current time and err as its outcome.
func audit(e auditEvent, err error) {
	e.Time = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	e.OK = err == nil
	if err != nil {
		e.Error = err.Error()
	}
	if len(e.SQL) <= maxAuditSQL {
		b, _ := json.Marshal(e)
		auditOut.Print(string(b))
		return
	}
	var parts []string
	for sql := e.SQL; sql != ""; {
		n := min(len(sql), maxAuditSQL)
		for n < len(sql) && !utf8.RuneStart(sql[n]) {
			n--
		}
		parts, sql = append(parts, sql[:n]), sql[n:]
	}
	e.ID, e.Parts = randomID()[:12], len(parts)
	for i, p := range parts {
		e.Part, e.SQL = i+1, p
		b, _ := json.Marshal(e)
		auditOut.Print(string(b))
	}
}

// done writes one SQL event on top of the base fields (user, server, db).
func (e auditEvent) done(event, sqlText string, start time.Time, err error, rows *int64) {
	e.Event, e.SQL, e.Rows = event, sqlText, rows
	e.Ms = float64(time.Since(start).Microseconds()) / 1000
	audit(e, err)
}

// auditConnector logs each connect (a SQL login as the user) and wraps the
// connections it hands out.
type auditConnector struct {
	driver.Connector
	base auditEvent
}

func (c auditConnector) Connect(ctx context.Context) (driver.Conn, error) {
	start := time.Now()
	conn, err := c.Connector.Connect(ctx)
	c.base.done("connect", "", start, err, nil)
	if err != nil {
		return nil, err
	}
	return &auditConn{Conn: conn, base: c.base}, nil
}

func (c auditConnector) Close() error {
	if cl, ok := c.Connector.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// auditConn wraps a driver connection. It forwards the optional interfaces
// database/sql probes for (Pinger, Validator, SessionResetter,
// NamedValueChecker) so pooling and parameter conversion work as with the
// bare driver, and deliberately lacks ExecerContext/QueryerContext, so every
// statement comes through PrepareContext and is seen. Ping is not logged: it
// is the app's own liveness check, not a statement of the user.
type auditConn struct {
	driver.Conn
	base auditEvent
}

func (c *auditConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *auditConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	start := time.Now()
	var st driver.Stmt
	var err error
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		st, err = p.PrepareContext(ctx, query)
	} else {
		st, err = c.Conn.Prepare(query)
	}
	if err != nil {
		c.base.done("prepare", query, start, err, nil)
		return nil, err
	}
	return &auditStmt{Stmt: st, base: c.base, sql: query}, nil
}

func (c *auditConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *auditConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	start := time.Now()
	var tx driver.Tx
	var err error
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = b.BeginTx(ctx, opts)
	} else {
		tx, err = c.Conn.Begin()
	}
	c.base.done("begin", "", start, err, nil)
	if err != nil {
		return nil, err
	}
	return auditTx{Tx: tx, base: c.base}, nil
}

func (c *auditConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *auditConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *auditConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *auditConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type auditTx struct {
	driver.Tx
	base auditEvent
}

func (t auditTx) Commit() error {
	start := time.Now()
	err := t.Tx.Commit()
	t.base.done("commit", "", start, err, nil)
	return err
}

func (t auditTx) Rollback() error {
	start := time.Now()
	err := t.Tx.Rollback()
	t.base.done("rollback", "", start, err, nil)
	return err
}

// auditStmt logs the execution of one prepared statement with its text.
type auditStmt struct {
	driver.Stmt
	base auditEvent
	sql  string
}

func (s *auditStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *auditStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	start := time.Now()
	var res driver.Result
	var err error
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		res, err = e.ExecContext(ctx, args)
	} else {
		res, err = s.Stmt.Exec(values(args))
	}
	// A bulk copy (mssql.CopyIn) takes one Exec per row and a final Exec
	// without arguments that sends the rest and returns the row count; that
	// one is logged, a line per row would drown the log.
	if len(args) > 0 && strings.HasPrefix(s.sql, "INSERTBULK") && err == nil {
		return res, err
	}
	var rows *int64
	if err == nil && res != nil {
		if n, e := res.RowsAffected(); e == nil {
			rows = &n
		}
	}
	s.base.done("exec", s.sql, start, err, rows)
	return res, err
}

func (s *auditStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

// QueryContext logs a refused query at once. An accepted one is logged when
// its result set is closed: errors such as a division by zero or a
// conversion failure arrive in the row stream, after the server accepted the
// query, and a query that was cut short is still one the user ran.
func (s *auditStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	start := time.Now()
	var rows driver.Rows
	var err error
	if q, ok := s.Stmt.(driver.StmtQueryContext); ok {
		rows, err = q.QueryContext(ctx, args)
	} else {
		rows, err = s.Stmt.Query(values(args))
	}
	if err != nil {
		s.base.done("query", s.sql, start, err, nil)
		return nil, err
	}
	return &auditRows{Rows: rows, base: s.base, sql: s.sql, start: start}, nil
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

func values(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

// auditRows counts the rows read and logs the query when the result set is
// closed. The column type interfaces are forwarded because handlers depend on
// DatabaseTypeName; database/sql only sees them on the outermost Rows.
type auditRows struct {
	driver.Rows
	base  auditEvent
	sql   string
	start time.Time
	n     int64
	err   error
	done  bool
}

func (r *auditRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	switch err {
	case nil:
		r.n++
	case io.EOF:
	default:
		r.err = err
	}
	return err
}

// Close logs the query. go-mssqldb drains the unread rest of the result in
// Close, so an error after the rows the handler read surfaces here.
func (r *auditRows) Close() error {
	err := r.Rows.Close()
	if !r.done {
		r.done = true
		if r.err == nil {
			r.err = err
		}
		n := r.n
		r.base.done("query", r.sql, r.start, r.err, &n)
	}
	return err
}

func (r *auditRows) HasNextResultSet() bool {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *auditRows) NextResultSet() error {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return io.EOF
}

func (r *auditRows) ColumnTypeDatabaseTypeName(i int) string {
	if c, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return c.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *auditRows) ColumnTypeLength(i int) (int64, bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return c.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *auditRows) ColumnTypeNullable(i int) (bool, bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return c.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *auditRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return c.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *auditRows) ColumnTypeScanType(i int) reflect.Type {
	if c, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return c.ColumnTypeScanType(i)
	}
	return reflect.TypeOf(new(any)).Elem()
}
