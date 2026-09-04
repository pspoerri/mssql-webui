# mssql-webui design

Date: 2026-09-04

## Goal

A single-binary web app for browsing and editing MS SQL databases with the
user's own Entra ID identity. The Go backend runs the OAuth flow, holds the
user's tokens server-side, and opens SQL connections with the user's access
token. The React frontend contains no auth code.

## Scope

In:
- Entra login; access gated by membership in one configured Entra group.
- List configured servers and the databases each user can see on them.
- Browse tables/views and columns.
- Page through rows; insert, update, delete rows in a grid, keyed by primary key.
- Secondary ad-hoc SQL console.
- Multi-stage Docker build producing one image.

Out (add when needed):
- Admin operations (create/drop databases, logins, roles, backups).
- Per-group database mapping; SQL permissions decide visibility.
- Deep links, virtual scrolling, Monaco editor, multi-result-set queries,
  optimistic concurrency, multi-instance session store.

## Repo layout

```
main.go        server, routes, static serving (embeds web/dist)
auth.go        login/callback/logout, session store, token refresh, middleware
sql.go         per-user connectors, schema/rows/query handlers, SQL builders
sql_test.go    quoteIdent, edit-batch builder, server URL parsing, value encoding tests
auth_test.go   ID token claim parsing and group check tests
web/           Vite + React + TypeScript, pnpm
Dockerfile     node build stage -> go build stage -> distroless runtime
README.md      app registration steps, env vars, network requirement
```

Go dependencies: `golang.org/x/oauth2`, `github.com/microsoft/go-mssqldb`.
Frontend dependencies: react, react-dom, vite, typescript. No UI framework,
router, or grid library.

## Configuration (env vars)

| Var | Meaning |
|---|---|
| `TENANT_ID` | Entra tenant |
| `CLIENT_ID`, `CLIENT_SECRET` | confidential "Web" app registration |
| `REDIRECT_URL` | e.g. `https://host/auth/callback` |
| `ALLOWED_GROUP_ID` | object ID of the Entra group allowed to use the app |
| `SQL_SERVERS` | comma-separated go-mssqldb URLs without credentials, e.g. `sqlserver://sql1.internal:1433?encrypt=true,sqlserver://sql2:1433?trustservercertificate=true`. Display name = host. |
| `LISTEN_ADDR` | default `:8080` |
| `DEV_USER` | dev only: skip Entra, run every request as this name, connect with the SQL login in each `SQL_SERVERS` URL |

Entra app registration requirements (documented in README):
- Platform: Web, redirect URI = `REDIRECT_URL`, client secret.
- API permission: Azure SQL Database → `user_impersonation` (delegated).
- Token configuration: emit `groups` claim, "groups assigned to the
  application", to avoid the 200-group overage. Assign the allowed group to
  the enterprise app; optionally set "user assignment required" so Entra
  refuses non-members before they reach us.

The backend needs TCP reachability to every server in `SQL_SERVERS` (peered
VNet, private endpoint, or VPN). Entra token auth works identically for Azure
SQL Database, Managed Instance, and Arc-enabled SQL Server 2022+.

## Auth flow

1. `GET /auth/login`: generate random `state`, store it in a short-lived
   cookie, redirect to Entra authorize endpoint with scopes
   `openid profile offline_access https://database.windows.net/user_impersonation`.
2. `GET /auth/callback`: verify `state`, exchange code via `oauth2.Config`.
   Parse the ID token claims (base64 JSON, no signature check: the token
   arrives over TLS directly from the token endpoint, which OIDC permits).
   Check `aud == CLIENT_ID`, `tid == TENANT_ID`, and `groups` contains
   `ALLOWED_GROUP_ID`; otherwise render a 403 page.
3. Create a session: random 32-byte ID → `{name, email, oauth2.TokenSource,
   conns map}` in an in-memory map guarded by a mutex. Set cookie `sid`,
   HttpOnly, Secure, SameSite=Lax. `ponytail:` single instance; swap the map
   for Redis if scaled out.
4. Middleware for `/api/*`: resolve session or 401.
5. `POST /auth/logout`: close the session's `sql.DB`s, delete the session,
   clear the cookie.

Token refresh is handled by `oauth2.TokenSource`; a refresh failure surfaces
as 401 from the API, which sends the user back through login.

## Dev mode

With `DEV_USER` set, `initAuth` creates one fixed session and skips the Entra
config; `withSession` serves that session to every request; `session.db`
uses `mssql.NewConnectorConfig` (SQL login from the URL) instead of the token
connector. `/auth/login` and `/auth/callback` are not registered. Lets the
schema browser, grid, and console be exercised against a local SQL Server in
Docker. Anyone reaching the port is that user: never set in production.

## SQL access

One `*sql.DB` per (session, server, database), created lazily, cached in the
session, closed on logout or after 30 minutes idle. Built with
`mssql.NewSecurityTokenConnector(config, tokenFunc)` where `tokenFunc` returns
`TokenSource.Token().AccessToken`, so new pooled connections always carry a
valid token. Per-database connections (not `USE`) so Azure SQL Database works.

### Endpoints (JSON, behind session middleware)

| Route | Returns |
|---|---|
| `GET /api/me` | `{name, email}` |
| `GET /api/servers` | `[{name, databases: [..], error?}]`; databases from `sys.databases` on `master`; a permission error yields `databases: []` plus `error` |
| `GET /api/s/{srv}/d/{db}/tables` | `[{schema, name, kind: "table"\|"view"}]` |
| `GET /api/s/{srv}/d/{db}/t/{schema}/{table}` | `{columns: [{name, type, nullable, identity, readonly}], pk: [..]}` |
| `GET .../t/{schema}/{table}/rows?offset=&limit=` | `{columns, rows: [[..]], hasMore}`; ordered by PK (`ORDER BY (SELECT NULL)` if none); limit default 100, capped at 500; `hasMore` comes from fetching limit+1 rows, no COUNT |
| `POST .../t/{schema}/{table}/rows` | body `{inserts: [{col: val}], updates: [{key: {pk: val}, set: {col: val}}], deletes: [{pk: val}]}`; one transaction; `{ok: true}` or `{error}` |
| `POST /api/s/{srv}/d/{db}/query` | body `{sql}`; statements starting with SELECT/WITH run as a query and return `{columns, rows}` (first result set, cap 1000 rows); anything else runs as exec and returns `{rowsAffected}` |

### Editing rules

- Updates/deletes require a primary key; the grid is read-only without one.
- Identity and computed columns are `readonly` and excluded from inserts/updates.
- Binary columns are read-only and rendered as base64.
- All identifiers pass through `quoteIdent` (`[name]`, `]` doubled). All
  values are bound parameters.
- Cell values travel as JSON strings or `null`; SQL Server implicit conversion
  handles numerics, dates, bits. In the grid, clearing a cell of a nullable
  column sends `null`, of a non-nullable column sends `""`. `ponytail:` typed
  conversion and an explicit NULL toggle if a type bites.
- Last write wins. `ponytail:` add a rowversion check if concurrent edits matter.

### Errors

SQL errors → 400 `{error: "<server message>"}`. Unknown server/db/table → 404.
Session missing or token refresh failed → 401. Everything else (network,
connect) → 500 `{error: message}`; the message is shown since users need it to
debug reachability, and only group members ever see it.

## Frontend

Two-pane layout, plain CSS.

- Header: app name, user name, Logout button.
- Left pane: tree servers → databases → tables/views. Clicking a database
  loads its tables. A "SQL" entry under each database opens the console.
- Right pane, table view: `<table>` with an `<input>` per editable cell,
  Prev/Next paging, "Add row", per-row delete checkbox, Save and Discard.
  Edits are local until Save posts the batch; a save error shows in a banner
  and keeps the edits. No-PK tables render read-only with a note.
- Right pane, console: `<textarea>`, Run button, read-only result table or
  rows-affected line.

`api()` fetch wrapper: JSON in/out; on 401 sets `window.location = '/auth/login'`.

Dev: Vite proxies `/api` and `/auth` to `http://localhost:8080`.
Build: `pnpm build` → `web/dist`, embedded with `//go:embed`; Go serves
`index.html` for any path not under `/api` or `/auth`.

## Docker

```
FROM node:22-alpine AS web    # npm i -g pnpm@11, pnpm install --frozen-lockfile, pnpm build
FROM golang:1.27-alpine AS build  # copy web/dist from web stage, CGO_ENABLED=0 go build
FROM gcr.io/distroless/static # copy binary, EXPOSE 8080, ENTRYPOINT
```

## Testing

`sql_test.go`: `quoteIdent` (with `]`), the edit-batch builder (one insert,
one update, one delete → expected SQL text and parameter order; empty key
rejected), `parseServers`, and `jsonValue` (decimal, GUID, binary).
`auth_test.go`: `parseIDToken` + `checkClaims` (member, non-member, wrong
audience, malformed token). `go vet` and `tsc -b` cover the rest. No frontend
tests. No SQL Server integration test: Entra token auth cannot be exercised
against a local container.
