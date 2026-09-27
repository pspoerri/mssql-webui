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
// filegroups, triggers or extended properties. Features that change what the
// table is (temporal, graph, sparse, ...) are named in a header comment
// instead of scripted. Add them when someone needs a script that recreates a
// table 1:1.
//
// A user without VIEW DEFINITION on the table sees its columns but not the
// text of computed columns, defaults, checks or index filters (the catalog
// returns NULL). Those are listed as hidden rather than silently left out, so
// the script never claims less than the table enforces.

type ddlCol struct {
	Name, Type, Collation, DefaultName, Default, Computed string
	Seed, Inc                                             string // IDENTITY; decimal seeds can exceed bigint
	Nullable, Identity, Persisted                         bool
}
type ddlIndex struct {
	Name, Filter                string
	PK, Unique, Clustered       bool
	UniqueConstraint, IgnoreDup bool
	Disabled                    bool
	Cols, Include               []string // quoted; Cols carry " DESC" when descending
}
type ddlFK struct {
	Name, RefTable, OnDelete, OnUpdate string
	Cols, RefCols                      []string // quoted
	Disabled                           bool
}
type ddlCheck struct {
	Name, Expr string
	Disabled   bool
}
type tableDef struct {
	Schema, Name string
	Cols         []ddlCol
	Indexes      []ddlIndex
	FKs          []ddlFK
	Checks       []ddlCheck
	Notes        []string // what the script leaves out, printed as a header comment
}

func renderDDL(d tableDef) string {
	tbl := quoteIdent(d.Schema) + "." + quoteIdent(d.Name)
	var lines, after []string
	for _, c := range d.Cols {
		l := "    " + quoteIdent(c.Name)
		if c.Computed != "" {
			l += " AS " + c.Computed
			if c.Persisted {
				l += " PERSISTED"
				if !c.Nullable {
					l += " NOT NULL"
				}
			}
			lines = append(lines, l)
			continue
		}
		l += " " + c.Type
		if c.Collation != "" {
			l += " COLLATE " + c.Collation
		}
		if c.Identity {
			l += fmt.Sprintf(" IDENTITY(%s,%s)", c.Seed, c.Inc)
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
		with := ""
		if ix.IgnoreDup {
			with = " WITH (IGNORE_DUP_KEY = ON)"
		}
		switch {
		case ix.PK:
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s PRIMARY KEY %s (%s)%s", quoteIdent(ix.Name), clustered, strings.Join(ix.Cols, ", "), with))
		case ix.UniqueConstraint:
			lines = append(lines, fmt.Sprintf("    CONSTRAINT %s UNIQUE %s (%s)%s", quoteIdent(ix.Name), clustered, strings.Join(ix.Cols, ", "), with))
		default:
			unique := ""
			if ix.Unique {
				unique = "UNIQUE "
			}
			s := fmt.Sprintf("CREATE %s%s INDEX %s ON %s (%s)", unique, clustered, quoteIdent(ix.Name), tbl, strings.Join(ix.Cols, ", "))
			if len(ix.Include) > 0 {
				s += " INCLUDE (" + strings.Join(ix.Include, ", ") + ")"
			}
			if ix.Filter != "" {
				s += " WHERE " + ix.Filter
			}
			indexes = append(indexes, s+with+";")
		}
		if ix.Disabled {
			after = append(after, fmt.Sprintf("ALTER INDEX %s ON %s DISABLE;", quoteIdent(ix.Name), tbl))
		}
	}
	for _, fk := range d.FKs {
		l := fmt.Sprintf("    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", quoteIdent(fk.Name), strings.Join(fk.Cols, ", "), fk.RefTable, strings.Join(fk.RefCols, ", "))
		if fk.OnDelete != "" && fk.OnDelete != "NO_ACTION" {
			l += " ON DELETE " + strings.ReplaceAll(fk.OnDelete, "_", " ")
		}
		if fk.OnUpdate != "" && fk.OnUpdate != "NO_ACTION" {
			l += " ON UPDATE " + strings.ReplaceAll(fk.OnUpdate, "_", " ")
		}
		lines = append(lines, l)
		if fk.Disabled {
			after = append(after, fmt.Sprintf("ALTER TABLE %s NOCHECK CONSTRAINT %s;", tbl, quoteIdent(fk.Name)))
		}
	}
	for _, ck := range d.Checks {
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s CHECK %s", quoteIdent(ck.Name), ck.Expr))
		if ck.Disabled {
			after = append(after, fmt.Sprintf("ALTER TABLE %s NOCHECK CONSTRAINT %s;", tbl, quoteIdent(ck.Name)))
		}
	}
	out := ""
	if len(d.Notes) > 0 {
		out = "-- Incomplete: this script leaves out\n--   " + strings.Join(d.Notes, "\n--   ") + "\n\n"
	}
	out += "CREATE TABLE " + tbl + " (\n" + strings.Join(lines, ",\n") + "\n);\n"
	if len(indexes) > 0 {
		out += "\n" + strings.Join(indexes, "\n") + "\n"
	}
	if len(after) > 0 {
		out += "\n" + strings.Join(after, "\n") + "\n"
	}
	return out
}

// scanRows runs query with obj and calls scan for each row.
func scanRows(ctx context.Context, db *sql.DB, query, obj string, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query, obj)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func loadTableDef(ctx context.Context, db *sql.DB, schema, table string) (tableDef, error) {
	d := tableDef{Schema: schema, Name: table}
	obj := quoteIdent(schema) + "." + quoteIdent(table)
	var hidden []string // definitions the user may not read

	// Alias types are schema-qualified: a bare name can bind to a different type.
	err := scanRows(ctx, db, `SELECT c.name, COALESCE(TYPE_NAME(c.user_type_id), ''),
		COALESCE(TYPE_NAME(c.system_type_id), TYPE_NAME(c.user_type_id), ''), c.max_length, c.precision, c.scale,
		COALESCE(CASE WHEN t.is_user_defined = 1 THEN SCHEMA_NAME(t.schema_id) END, ''),
		CASE WHEN c.collation_name = CAST(DATABASEPROPERTYEX(DB_NAME(), 'Collation') AS sysname) OR t.is_user_defined = 1 THEN '' ELSE COALESCE(c.collation_name, '') END,
		c.is_nullable, c.is_identity, COALESCE(CONVERT(varchar(40), ic.seed_value), ''), COALESCE(CONVERT(varchar(40), ic.increment_value), ''),
		df.name, df.definition, c.is_computed, cc.definition, COALESCE(cc.is_persisted, 0)
		FROM sys.columns c
		LEFT JOIN sys.types t ON t.user_type_id = c.user_type_id
		LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
		LEFT JOIN sys.default_constraints df ON df.parent_object_id = c.object_id AND df.parent_column_id = c.column_id
		LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		WHERE c.object_id = OBJECT_ID(@p1) ORDER BY c.column_id`, obj, func(rows *sql.Rows) error {
		var c ddlCol
		var base, typeSchema string
		var maxLen, prec, scale int
		var computed bool
		var dfName, dfDef, ccDef sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &base, &maxLen, &prec, &scale, &typeSchema, &c.Collation, &c.Nullable, &c.Identity,
			&c.Seed, &c.Inc, &dfName, &dfDef, &computed, &ccDef, &c.Persisted); err != nil {
			return err
		}
		if typeSchema != "" {
			c.Type = quoteIdent(typeSchema) + "." + quoteIdent(c.Type)
		} else {
			c.Type = typeLabel(c.Type, base, maxLen, prec, scale)
		}
		c.DefaultName, c.Default = dfName.String, dfDef.String
		if dfName.Valid && !dfDef.Valid {
			hidden = append(hidden, "the default of "+quoteIdent(c.Name))
		}
		c.Computed = ccDef.String
		if computed && !ccDef.Valid {
			c.Computed = "(NULL) /* expression hidden */"
			hidden = append(hidden, "the expression of computed column "+quoteIdent(c.Name))
		}
		d.Cols = append(d.Cols, c)
		return nil
	})
	if err != nil {
		return d, err
	}
	if len(d.Cols) == 0 {
		return d, errNotFound
	}

	// One row per index column, grouped by index name in key order. The
	// partitioning column of an aligned index is listed with key_ordinal 0
	// but is not part of the key; hypothetical indexes (from the tuning
	// advisor) do not exist.
	err = scanRows(ctx, db, `SELECT i.name, i.is_primary_key, i.is_unique_constraint, i.is_unique, i.type, i.ignore_dup_key, i.is_disabled,
		i.has_filter, i.filter_definition, c.name, ic.is_descending_key, ic.is_included_column
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = OBJECT_ID(@p1) AND i.type IN (1, 2) AND i.is_hypothetical = 0 AND (ic.key_ordinal > 0 OR ic.is_included_column = 1)
		ORDER BY i.index_id, ic.is_included_column, ic.key_ordinal, ic.index_column_id`, obj, func(rows *sql.Rows) error {
		var ix ddlIndex
		var typ int
		var hasFilter, desc, incl bool
		var filter sql.NullString
		var col string
		if err := rows.Scan(&ix.Name, &ix.PK, &ix.UniqueConstraint, &ix.Unique, &typ, &ix.IgnoreDup, &ix.Disabled,
			&hasFilter, &filter, &col, &desc, &incl); err != nil {
			return err
		}
		if n := len(d.Indexes); n == 0 || d.Indexes[n-1].Name != ix.Name {
			ix.Clustered, ix.Filter = typ == 1, filter.String
			if hasFilter && !filter.Valid {
				hidden = append(hidden, "the filter of index "+quoteIdent(ix.Name))
			}
			d.Indexes = append(d.Indexes, ix)
		}
		last := &d.Indexes[len(d.Indexes)-1]
		col = quoteIdent(col)
		switch {
		case incl:
			last.Include = append(last.Include, col)
		case desc:
			last.Cols = append(last.Cols, col+" DESC")
		default:
			last.Cols = append(last.Cols, col)
		}
		return nil
	})
	if err != nil {
		return d, err
	}

	// A referenced table the user cannot see comes back without a name; the
	// key is named in the header rather than silently dropped.
	var invisible []string
	err = scanRows(ctx, db, `SELECT fk.name, SCHEMA_NAME(ro.schema_id), ro.name, fk.delete_referential_action_desc,
		fk.update_referential_action_desc, fk.is_disabled, pc.name, rc.name
		FROM sys.foreign_keys fk
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		LEFT JOIN sys.objects ro ON ro.object_id = fk.referenced_object_id
		LEFT JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE fk.parent_object_id = OBJECT_ID(@p1) ORDER BY fk.name, fkc.constraint_column_id`, obj, func(rows *sql.Rows) error {
		var fk ddlFK
		var rs, rt, rc sql.NullString
		var pc string
		if err := rows.Scan(&fk.Name, &rs, &rt, &fk.OnDelete, &fk.OnUpdate, &fk.Disabled, &pc, &rc); err != nil {
			return err
		}
		if !rt.Valid || !rc.Valid {
			if n := len(invisible); n == 0 || invisible[n-1] != fk.Name {
				invisible = append(invisible, fk.Name)
			}
			return nil
		}
		if n := len(d.FKs); n == 0 || d.FKs[n-1].Name != fk.Name {
			fk.RefTable = quoteIdent(rs.String) + "." + quoteIdent(rt.String)
			d.FKs = append(d.FKs, fk)
		}
		last := &d.FKs[len(d.FKs)-1]
		last.Cols = append(last.Cols, quoteIdent(pc))
		last.RefCols = append(last.RefCols, quoteIdent(rc.String))
		return nil
	})
	if err != nil {
		return d, err
	}

	err = scanRows(ctx, db, `SELECT name, definition, is_disabled FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID(@p1) ORDER BY name`, obj, func(rows *sql.Rows) error {
		var ck ddlCheck
		var expr sql.NullString
		if err := rows.Scan(&ck.Name, &expr, &ck.Disabled); err != nil {
			return err
		}
		if !expr.Valid {
			hidden = append(hidden, "check "+quoteIdent(ck.Name))
			return nil
		}
		ck.Expr = expr.String
		d.Checks = append(d.Checks, ck)
		return nil
	})
	if err != nil {
		return d, err
	}

	// Table kinds and features this script does not reproduce.
	var name string
	var temporal, memory, node, edge bool
	var generated, sparse, typedXML, otherIndexes, partitioned int
	err = db.QueryRowContext(ctx, `SELECT SCHEMA_NAME(t.schema_id), t.name, CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS bit),
		t.is_memory_optimized, t.is_node, t.is_edge,
		(SELECT COUNT(*) FROM sys.columns c WHERE c.object_id = t.object_id AND c.generated_always_type <> 0),
		(SELECT COUNT(*) FROM sys.columns c WHERE c.object_id = t.object_id AND (c.is_sparse = 1 OR c.is_column_set = 1)),
		(SELECT COUNT(*) FROM sys.columns c WHERE c.object_id = t.object_id AND c.xml_collection_id <> 0),
		(SELECT COUNT(*) FROM sys.indexes i WHERE i.object_id = t.object_id AND i.type NOT IN (0, 1, 2)),
		(SELECT COUNT(*) FROM sys.indexes i JOIN sys.partition_schemes ps ON ps.data_space_id = i.data_space_id WHERE i.object_id = t.object_id)
		FROM sys.tables t WHERE t.object_id = OBJECT_ID(@p1)`, obj).Scan(&d.Schema, &name, &temporal, &memory, &node, &edge,
		&generated, &sparse, &typedXML, &otherIndexes, &partitioned)
	if err != nil {
		return d, err
	}
	d.Name = name // the catalog's spelling, not the URL's
	for _, n := range []struct {
		on   bool
		note string
	}{
		{temporal, "system versioning (PERIOD FOR SYSTEM_TIME, GENERATED ALWAYS, the history table)"},
		{!temporal && generated > 0, "GENERATED ALWAYS columns (ledger), listed here as plain columns"},
		{node || edge, "AS NODE / AS EDGE: this is a graph table; its internal graph columns are listed as plain columns"},
		{memory, "MEMORY_OPTIMIZED and its hash indexes"},
		{sparse > 0, "SPARSE columns and column sets, listed here as plain columns"},
		{typedXML > 0, "XML schema collections of typed xml columns"},
		{otherIndexes > 0, "columnstore, XML, spatial and hash indexes"},
		{partitioned > 0, "partitioning: the table and indexes are created here unpartitioned"},
		{len(hidden) > 0, "(you lack VIEW DEFINITION on this table) " + strings.Join(hidden, ", ")},
		{len(invisible) > 0, "foreign keys referencing tables you cannot see: " + strings.Join(invisible, ", ")},
	} {
		if n.on {
			d.Notes = append(d.Notes, n.note)
		}
	}
	return d, nil
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
	} else if !viewDef.Valid {
		ddl = "-- The definition is not available: the view is encrypted, or you lack VIEW DEFINITION on it."
	}
	writeJSON(w, http.StatusOK, map[string]any{"ddl": ddl})
}
