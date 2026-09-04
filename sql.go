package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
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
