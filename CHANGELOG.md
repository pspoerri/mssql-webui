# Changelog

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
