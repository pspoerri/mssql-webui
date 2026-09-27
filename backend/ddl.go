package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
)

// Table definition as CREATE statements, reconstructed from the catalog views.
// Views come back verbatim from OBJECT_DEFINITION.
// ponytail: columns, keys, foreign keys, checks and indexes; no partition schemes,
// filegroups, triggers or extended properties. Add them when someone needs a
// script that recreates a table 1:1.

type ddlCol struct {
	Name, Type, Collation, DefaultName, Default, Computed string
	Nullable, Identity, Persisted                         bool
	Seed, Inc                                             int64
}
type ddlIndex struct {
	Name, Filter          string
	PK, Unique, Clustered bool
	UniqueConstraint      bool
	Cols, Include         []string // Cols carry a " DESC" suffix when descending
}
type ddlFK struct {
	Name, RefTable, OnDelete, OnUpdate string
	Cols, RefCols                      []string
}
type ddlCheck struct{ Name, Expr string }
type tableDef struct {
	Schema, Name string
	Cols         []ddlCol
	Indexes      []ddlIndex
	FKs          []ddlFK
	Checks       []ddlCheck
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		name, desc, _ := strings.Cut(c, " ")
		q[i] = quoteIdent(name)
		if desc != "" {
			q[i] += " " + desc
		}
	}
	return strings.Join(q, ", ")
}

func renderDDL(d tableDef) string {
	tbl := quoteIdent(d.Schema) + "." + quoteIdent(d.Name)
	var lines []string
	for _, c := range d.Cols {
		l := "    " + quoteIdent(c.Name)
		if c.Computed != "" {
			l += " AS " + c.Computed
			if c.Persisted {
				l += " PERSISTED"
			}
			lines = append(lines, l)
			continue
		}
		l += " " + c.Type
		if c.Collation != "" {
			l += " COLLATE " + c.Collation
		}
		if c.Identity {
			l += fmt.Sprintf(" IDENTITY(%d,%d)", c.Seed, c.Inc)
		}
		if c.Nullable {
			l += " NULL"
		} else {
			l += " NOT NULL"
		}
		if c.Default != "" {
			l += " CONSTRAINT " + quoteIdent(c.DefaultName) + " DEFAULT " + c.Default
		}
		lines = append(lines, l)
	}
	var indexes []string
	for _, ix := range d.Indexes {
		clustered := "NONCLUSTERED"
		if ix.Clustered {
			clustered = "CLUSTERED"
		}
		switch {
		case ix.PK:
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s PRIMARY KEY %s (%s)", quoteIdent(ix.Name), clustered, quoteList(ix.Cols)))
		case ix.UniqueConstraint:
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s UNIQUE %s (%s)", quoteIdent(ix.Name), clustered, quoteList(ix.Cols)))
		default:
			unique := ""
			if ix.Unique {
				unique = "UNIQUE "
			}
			s := fmt.Sprintf("CREATE %s%s INDEX %s ON %s (%s)", unique, clustered, quoteIdent(ix.Name), tbl, quoteList(ix.Cols))
			if len(ix.Include) > 0 {
				s += " INCLUDE (" + quoteList(ix.Include) + ")"
			}
			if ix.Filter != "" {
				s += " WHERE " + ix.Filter
			}
			indexes = append(indexes, s+";")
		}
	}
	for _, fk := range d.FKs {
		l := fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", quoteIdent(fk.Name), quoteList(fk.Cols), fk.RefTable, quoteList(fk.RefCols))
		if fk.OnDelete != "" && fk.OnDelete != "NO_ACTION" {
			l += " ON DELETE " + strings.ReplaceAll(fk.OnDelete, "_", " ")
		}
		if fk.OnUpdate != "" && fk.OnUpdate != "NO_ACTION" {
			l += " ON UPDATE " + strings.ReplaceAll(fk.OnUpdate, "_", " ")
		}
		lines = append(lines, l)
	}
	for _, ck := range d.Checks {
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s CHECK %s", quoteIdent(ck.Name), ck.Expr))
	}
	out := "CREATE TABLE " + tbl + " (\n" + strings.Join(lines, ",\n") + "\n);\n"
	if len(indexes) > 0 {
		out += "\n" + strings.Join(indexes, "\n") + "\n"
	}
	return out
}

func loadTableDef(ctx context.Context, db *sql.DB, schema, table string) (tableDef, error) {
	d := tableDef{Schema: schema, Name: table}
	obj := quoteIdent(schema) + "." + quoteIdent(table)
	rows, err := db.QueryContext(ctx, `SELECT c.name, COALESCE(TYPE_NAME(c.user_type_id), ''),
		COALESCE(TYPE_NAME(c.system_type_id), TYPE_NAME(c.user_type_id), ''), c.max_length, c.precision, c.scale,
		CASE WHEN c.collation_name = CAST(DATABASEPROPERTYEX(DB_NAME(), 'Collation') AS sysname) THEN '' ELSE COALESCE(c.collation_name, '') END,
		c.is_nullable, c.is_identity, COALESCE(CONVERT(bigint, ic.seed_value), 0), COALESCE(CONVERT(bigint, ic.increment_value), 0),
		COALESCE(df.name, ''), COALESCE(df.definition, ''), COALESCE(cc.definition, ''), COALESCE(cc.is_persisted, 0)
		FROM sys.columns c
		LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
		LEFT JOIN sys.default_constraints df ON df.parent_object_id = c.object_id AND df.parent_column_id = c.column_id
		LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		WHERE c.object_id = OBJECT_ID(@p1) ORDER BY c.column_id`, obj)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var c ddlCol
		var base string
		var maxLen, prec, scale int
		if err := rows.Scan(&c.Name, &c.Type, &base, &maxLen, &prec, &scale, &c.Collation, &c.Nullable, &c.Identity,
			&c.Seed, &c.Inc, &c.DefaultName, &c.Default, &c.Computed, &c.Persisted); err != nil {
			rows.Close()
			return d, err
		}
		c.Type = typeLabel(c.Type, base, maxLen, prec, scale)
		d.Cols = append(d.Cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	if len(d.Cols) == 0 {
		return d, errNotFound
	}

	// One row per index column; grouped by index name in key order.
	rows, err = db.QueryContext(ctx, `SELECT i.name, i.is_primary_key, i.is_unique_constraint, i.is_unique, i.type,
		COALESCE(i.filter_definition, ''), c.name, ic.is_descending_key, ic.is_included_column
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = OBJECT_ID(@p1) AND i.type IN (1, 2) ORDER BY i.index_id, ic.is_included_column, ic.key_ordinal, ic.index_column_id`, obj)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var name, filter, col string
		var pk, uc, unique, desc, incl bool
		var typ int
		if err := rows.Scan(&name, &pk, &uc, &unique, &typ, &filter, &col, &desc, &incl); err != nil {
			rows.Close()
			return d, err
		}
		if n := len(d.Indexes); n == 0 || d.Indexes[n-1].Name != name {
			d.Indexes = append(d.Indexes, ddlIndex{Name: name, PK: pk, UniqueConstraint: uc, Unique: unique, Clustered: typ == 1, Filter: filter})
		}
		ix := &d.Indexes[len(d.Indexes)-1]
		if incl {
			ix.Include = append(ix.Include, col)
		} else {
			if desc {
				col += " DESC"
			}
			ix.Cols = append(ix.Cols, col)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}

	rows, err = db.QueryContext(ctx, `SELECT fk.name, SCHEMA_NAME(ro.schema_id), ro.name,
		fk.delete_referential_action_desc, fk.update_referential_action_desc, pc.name, rc.name
		FROM sys.foreign_keys fk
		JOIN sys.objects ro ON ro.object_id = fk.referenced_object_id
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE fk.parent_object_id = OBJECT_ID(@p1) ORDER BY fk.name, fkc.constraint_column_id`, obj)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var name, rs, rt, del, upd, pc, rc string
		if err := rows.Scan(&name, &rs, &rt, &del, &upd, &pc, &rc); err != nil {
			rows.Close()
			return d, err
		}
		if n := len(d.FKs); n == 0 || d.FKs[n-1].Name != name {
			d.FKs = append(d.FKs, ddlFK{Name: name, RefTable: quoteIdent(rs) + "." + quoteIdent(rt), OnDelete: del, OnUpdate: upd})
		}
		fk := &d.FKs[len(d.FKs)-1]
		fk.Cols = append(fk.Cols, pc)
		fk.RefCols = append(fk.RefCols, rc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}

	rows, err = db.QueryContext(ctx, `SELECT name, definition FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID(@p1) ORDER BY name`, obj)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var ck ddlCheck
		if err := rows.Scan(&ck.Name, &ck.Expr); err != nil {
			return d, err
		}
		d.Checks = append(d.Checks, ck)
	}
	return d, rows.Err()
}

func handleDDL(w http.ResponseWriter, r *http.Request, s *session) {
	db, err := s.db(r.Context(), r.PathValue("srv"), r.PathValue("db"))
	if err != nil {
		fail(w, err)
		return
	}
	var kind, viewDef sql.NullString
	err = db.QueryRowContext(r.Context(), `SELECT type, OBJECT_DEFINITION(object_id) FROM sys.objects WHERE object_id = OBJECT_ID(@p1)`, objName(r)).Scan(&kind, &viewDef)
	if err == sql.ErrNoRows {
		err = errNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	ddl := viewDef.String
	if strings.TrimSpace(kind.String) == "U" {
		d, err := loadTableDef(r.Context(), db, r.PathValue("schema"), r.PathValue("table"))
		if err != nil {
			fail(w, err)
			return
		}
		ddl = renderDDL(d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ddl": ddl})
}
