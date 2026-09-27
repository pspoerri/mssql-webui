# Changelog

## v0.6.0 — 2026-09-27

- CSV import handles files of millions of rows: the upload returns at once and the import
  runs as a background job, shown in the sidebar with upload percentage, type-check and
  insert progress, row count and a Cancel button (a canceled or failed import leaves nothing
  behind). Rows go in by bulk copy instead of batched INSERTs (1M rows: 3 s instead of 129 s
  locally), and a new table is committed before its rows, so the tree does not hang for other
  users while an import runs. Before, a long import outlived the proxy's request timeout and
  was rolled back without a visible error.
- Codes with a leading zero (zip, phone, account numbers) are imported as text instead of
  losing the zero as numbers.
- A paused serverless database no longer holds requests for up to two minutes: the server
  answers 503 at once and the UI shows "database … is paused and resuming" in the header
  while it retries.
- SQL console shows every result of a batch in order, like SSMS: each result set, rows-affected
  counts, PRINT messages and the error with the results before it. Before, only batches starting
  with SELECT/WITH showed rows (not one starting with a comment, DECLARE or EXEC), later errors
  in a batch were hidden, and a result over 1000 rows could silently cancel the statements after
  it. Each result set has a Download CSV button for all its rows (it runs the batch again, and
  asks first if the batch changed data). A transaction a batch leaves open is rolled back and
  reported instead of holding its locks.
- Definition panel (unreleased since v0.5.0) fixed after a review against a live server: a user
  with only SELECT got a 500 on any table with a CHECK and otherwise a script that silently
  dropped defaults, index filters and computed expressions; these are now listed as hidden in a
  header comment. Key, INCLUDE and foreign-key columns with spaces in their names are quoted
  correctly; alias types are schema-qualified; the partitioning column no longer appears as a key
  column; hypothetical indexes are skipped; disabled indexes and constraints, IGNORE_DUP_KEY and
  PERSISTED NOT NULL are scripted; IDENTITY seeds beyond bigint work; a foreign key to a table the
  user cannot see is named instead of vanishing; temporal, graph, ledger, memory-optimized,
  partitioned and sparse tables, typed XML and columnstore/XML/spatial indexes are named in the
  header instead of being scripted as if they were plain. The panel shows Loading…, ignores a
  second click, and stays within the window; long errors above the grid wrap instead of running
  off the right edge.
- `make seed` creates and fills database `demo` instead of `master`, runs sqlcmd with
  QUOTED_IDENTIFIER on (the filtered index failed without it) and can be re-run.
- End-to-end tests: `make e2e` drives the app in headless Chromium through a data steward's
  workflow on an existing database (browse, search, edit, definitions, CSV export/import, console,
  resuming database) against `make run-sqlserver`.
- Cross-site requests that change state are refused (Go's CrossOriginProtection): before, a
  page on a same-site sibling host could POST SQL with the user's cookie, and in dev mode any web
  page could. Dev mode also answers only requests addressed to localhost, which stops DNS
  rebinding.
- Sign-in uses PKCE, so an authorization code stolen from another login cannot be exchanged.
- The audit log keeps every statement whole: one over 8 KB is written across consecutive lines
  sharing an `id` (`part`/`parts`) instead of being cut, which could hide a statement's end.
- Clearing a number, date or bit cell sends NULL: a NOT NULL column refuses it instead of
  silently storing 0, 1900-01-01 or false.
- Tables with a binary primary key can be edited (the base64 shown in the grid is decoded
  before it is bound), and a downloaded CSV with binary or CLR columns (varbinary, image,
  geography, hierarchyid, ...) imports back.
- A database name that fails to open no longer leaves a connection pool behind for the rest of
  the session, and a request still running during logout cannot open new pools; a data race on
  the session's last-seen time is gone.
- `make run` works with the dev-mode `.env`: it publishes on the host's loopback and listens on
  all interfaces inside the container (see README for reaching SQL Server from the container).
- CI runs `make e2e` against a SQL Server service container.
- Errors from a proxy in front of the app (e.g. 413 for an upload over its size limit, 504
  for a timeout) are shown with their status instead of an empty message.

## v0.5.0 — 2026-09-04

- Audit log on stdout: every login and logout (with why a session ended), every SQL
  connection opened and every statement, commit and rollback is written as one JSON line
  with the user, server, database, statement text, duration, row count and the error if it
  failed. Diagnostics stay on stderr, so a collector can take stdout as the audit stream.
  Statements are caught at the database driver, so nothing reaches SQL Server unlogged;
  parameter values and result contents are not recorded.
- No-access errors say so: opening a database the user cannot access answers 403
  "you have no access to database …" instead of the raw "Login failed for user
  '<token-identified principal>'" text, and a server where the login cannot connect
  to `master` explains that databases cannot be listed and points at the
  `databases=` URL parameter instead of showing the bare token error.
- The "Import CSV (creates table)…" tree item only appears on databases where the
  user can write (`CREATE TABLE` or `INSERT` permission, or membership in
  `db_owner`/`db_datawriter`/`db_ddladmin`).
- Import CSV: a database's tree menu can load a CSV file into a table. A new table's
  column types are inferred as the narrowest type all values fit (bit, int, bigint, float,
  date, datetime2, datetimeoffset, else nvarchar); importing into an existing table appends
  the rows after a confirmation, matching header columns case-insensitively and skipping
  identity/computed/rowversion columns so a downloaded CSV imports back. Empty fields become
  NULL; semicolon/tab delimiters and a UTF-8 BOM are handled; one transaction either way.
  Uploads are spooled to a temp file and streamed from there in batched INSERTs, so memory
  stays flat regardless of file size (capped at 2 GB, up from 100 MB in memory). The tree
  menu item is labelled "Import CSV (creates table)…", and a spinner replaces the "loading…"
  text while a database's tables load or an import runs.
- Servers reachable without `master` access: a `databases=db1|db2` parameter on a
  `SQL_SERVERS` URL lists those databases for users whose `master` login fails
  ("Login failed for user '<token-identified principal>'"), instead of an error and an
  empty list. Each is probed so inaccessible ones are greyed out; a server whose listing
  failed is greyed out too.
- Group check failures now say why: missing groups claim, groups overage, or not a member —
  instead of a bare "not a member of the allowed group".

- Sessions end after 30 minutes without a request (in addition to the 12-hour cap); a sweeper
  drops them every minute and closes their SQL connections. A visible tab pings every 5 minutes
  to stay signed in; hidden tabs time out. `SECURITY.md` describes the token
  flow; the session-to-connection binding is now a named method with a test.

## v0.4.0 — 2026-09-04

- Redesign: one token palette (petrol accent for selection, amber for pending changes, red for
  delete) with a dark theme following the OS; data in monospace with numbers right-aligned;
  object icons in the tree (server, database, table, view, console) with indent guides;
  breadcrumb and tab title follow the selection; keyboard keys, help sections, favicon.
- Sidebar: drag its right edge to resize (double-click resets), header button hides it; both
  remembered.
- Pin icon in column headers: pinned columns move to the left edge in pin order and stay visible
  while scrolling sideways. Key columns are pinned by default.
- Columns open wide enough to show their loaded values in full (up to 120 characters).
- Save button shows how many rows it will write; column tooltips name the primary key.

## v0.3.0 — 2026-09-04

- Column header icons: sort ascending/descending/off, and a filter popover
  (starts with, contains, equals) that writes `col^v` / `col~v` / `col=v` terms into the
  search box. Filters stack across columns; clicking a lit filter icon removes it.
  Sort and filters are part of the URL.
- Search: quotes keep spaces together (`name='User 1002'`), bare words must all occur
  in the same column.
- Values render identically in the grid, console, CSV and text search, and can be
  typed back in that form: dates/times in SQL style 121, bit as true/false, money with
  four decimals, bigint and decimal as exact strings, binary as base64.
- Column types with length/precision on header and cell hover and in the field bar.
- Enter saves all pending changes (Ctrl+Enter in a multi-line editor); Esc reverts the
  current cell; the field bar has Revert and close, and is multi-line only for text types.

## v0.2.0 — 2026-09-04

- Column resizing: drag a header's right edge; double-click to fit the loaded content
  (capped at 120 characters), double-click again to fit the label. Cells show the full
  value on hover.
- Primary-key columns and the delete checkbox stay pinned when scrolling sideways;
  edited cells are highlighted.
- Field editor bar: the focused cell is mirrored in a textarea above the table with its
  row key. Toolbar and header stay fixed while scrolling.
- Scroll position in the URL (`?row=N`, per search); links open at the same row.
- Refresh button next to each server name.
- `docs/big-table.sql`: a 2000-row, 20-column test table.

## v0.1.0 — 2026-09-04

First release.

- Browse SQL Server databases with your Entra ID identity; SQL permissions apply per user.
  Dev mode (`DEV_USER`) uses SQL logins instead.
- Server/database/schema tree: create schemas and databases, hide system databases,
  greyed-out databases you cannot access, serverless databases are woken up.
- Table grid: rows load as you scroll; search by word or `col=value`; the table and
  search are encoded in the URL for bookmarking and deep links.
- Editing: tables with a primary key allow cell edits, deletes and inserts; all pending
  changes are saved in one transaction. Tables without a primary key are append-only,
  views are read-only. Unsaved changes highlight the Save button and the footer status,
  and leaving the page, switching tables or logging out asks for confirmation.
- Download any table or view as CSV.
- SQL console for ad-hoc statements.
- Help page linking to https://github.com/pspoerri/mssql-webui.
- Footer shows the build version (`git describe`, via `make build` / `make image`).
- Sessions expire 12 hours after login.
- Single Go binary with the UI embedded; Docker/Podman image; Makefile targets.
