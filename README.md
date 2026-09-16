# mssql-webui

Browse and edit MS SQL databases in the browser with your own Entra ID
identity. One Go binary serves the API and the React UI; SQL connections
use the logged-in user's Entra access token, so SQL permissions apply
per user.
[SECURITY.md](SECURITY.md) shows how the token travels.

## Entra app registration

1. Entra ID → App registrations → New. Platform **Web**, redirect URI
   `https://<host>/auth/callback` (for local dev also
   `http://localhost:5173/auth/callback`).
2. Certificates & secrets → new client secret → `CLIENT_SECRET`.
3. API permissions → Add → APIs my organization uses → **Azure SQL Database**
   → Delegated → `user_impersonation`. Grant admin consent if your tenant
   requires it.
4. (Optional, only if using `ALLOWED_GROUP_ID`) Token configuration → Add
   groups claim → **Security groups**, and tick **Groups assigned to the
   application** so the claim never overflows.
5. Enterprise applications → this app → Users and groups → assign the group
   you will put in `ALLOWED_GROUP_ID`, or assign users directly and set
   Properties → **Assignment required** so Entra refuses everyone else.

Every server must accept Entra logins, and each user needs a database user
(`CREATE USER [name@tenant] FROM EXTERNAL PROVIDER`) or group login on the
databases they should see. Listing databases queries `sys.databases` on
`master`, so a user who also exists in `master` sees the full list. A user
who only exists in specific databases fails there with `Login failed for
user '<token-identified principal>'` — either create the user in `master`
too, or add `databases=db1|db2` to the server URL:

```
SQL_SERVERS='sqlserver://sql1.database.windows.net:1433?encrypt=true&databases=app1|app2'
```

When the `master` login fails, those databases are listed instead ('|'
separates names because ',' separates servers). Each one is probed with a
short connect, so databases the user cannot open are greyed out here too.
Databases where `HAS_DBACCESS()` returns 0 are greyed out, and so is a
server whose listing failed entirely; system databases are hidden unless
toggled on at the bottom of the tree. A paused serverless database is
retried for up to two minutes while it resumes.

If login is refused with a group error, the message says why: *no groups
claim* means the app registration lacks the claim (step 4 above); *group
list overflowed* means the claim was replaced by an overage reference —
tick **Groups assigned to the application**; *not a member* means exactly
that. `ALLOWED_GROUP_ID` must be the group's **object ID** (a GUID), and
users must be members of that group — assigning a user directly to the
enterprise app does not make them a member; either add them to the group,
or unset `ALLOWED_GROUP_ID` and restrict access with **Assignment
required** alone.

## Configuration

| Env var | Meaning |
|---|---|
| `TENANT_ID` | Entra tenant ID (the directory GUID, not the domain name) |
| `CLIENT_ID`, `CLIENT_SECRET` | from the app registration |
| `REDIRECT_URL` | `https://<host>/auth/callback` |
| `ALLOWED_GROUP_ID` | optional; object ID of the Entra group allowed in. Unset means any user in the tenant who can sign in to the app (use **Assignment required** to restrict at Entra) |
| `SQL_SERVERS` | comma-separated go-mssqldb URLs — without credentials in Entra mode, with a SQL login in dev mode, e.g. `sqlserver://sql1.internal:1433?encrypt=true,sqlserver://sql2:1433?trustservercertificate=true`. An optional `databases=db1|db2` parameter lists the databases shown to users whose `master` login fails |
| `LISTEN_ADDR` | default `:8080` |
| `DEV_USER` | skip Entra; run as this user with SQL logins from `SQL_SERVERS`. Dev only. |

The process needs TCP reachability to every server (peered VNet, private
endpoint, or VPN).

## Audit log

Every login and logout, every SQL connection opened and every statement is
written to **stdout** as one JSON object per line, so a log collector can take
stdout as the audit stream; diagnostics go to stderr. Each line names the user
and says whether the action succeeded:

```
{"time":"2026-09-16T09:12:03.412Z","event":"login","user":"ann@example.com","name":"Ann","ok":true}
{"time":"2026-09-16T09:12:04.007Z","event":"connect","user":"ann@example.com","server":"sql1.internal","db":"app1","ok":true,"ms":180.3}
{"time":"2026-09-16T09:12:04.020Z","event":"query","user":"ann@example.com","server":"sql1.internal","db":"app1","ok":true,"ms":12.6,"rows":101,"sql":"SELECT * FROM [dbo].[orders] ORDER BY [id] OFFSET @p1 ROWS FETCH NEXT @p2 ROWS ONLY"}
{"time":"2026-09-16T09:13:10.551Z","event":"exec","user":"ann@example.com","server":"sql1.internal","db":"app1","ok":false,"error":"The UPDATE statement conflicted with the CHECK constraint \"ck_qty\".","ms":3.1,"sql":"UPDATE [dbo].[orders] SET [qty] = @p1 WHERE [id] = @p2"}
{"time":"2026-09-16T09:40:00.000Z","event":"logout","user":"ann@example.com","name":"Ann","reason":"idle","ok":true}
```

| `event` | Meaning |
|---|---|
| `login` | the Entra callback. `ok:false` with `error` for a refused login (bad state, Entra error, wrong tenant or audience, not in the group); `user` is set once the id_token got that far |
| `logout` | the session ended; `reason` is `user` (logout button), `idle` (30 min without a request), `expired` (12 h after login) or `relogin` (replaced by a new login from the same browser) |
| `connect` | a SQL connection opened as the user; a refused one carries the server's message, e.g. `Login failed for user` |
| `query`, `exec` | one statement; `rows` is rows read or rows affected. A query is logged when its result set is closed, so an error that arrives among the rows counts as a failure |
| `begin`, `commit`, `rollback` | the transaction around grid edits and CSV imports; a statement that succeeded and was then rolled back shows up as such |

Statement text is logged (cut at 8 KB with `"truncated":true`); parameter
values and result contents are not. The liveness ping before a request is not
a statement and is not logged. In dev mode `user` is `DEV_USER`. Statements
are caught at the database driver, so no request path can reach SQL Server
without a line. To keep the stream, redirect stdout (`./mssql-webui
>> audit.jsonl`); with a container, `docker logs <name> 2>/dev/null` prints
stdout alone, and log drivers tag each line with its source.

## Using it

Pick a table in the tree; the URL (`/s/{server}/d/{db}/t/{schema}/{table}`)
can be bookmarked or shared. Rows load 100 at a time as you scroll. The search
box filters rows: bare words must all occur in one column (`User 1001` finds
that name); `col=value` matches one column exactly, `col^value` a prefix,
`col~value` a substring; quotes keep spaces together (`name='User 1002'`); all
terms must match (`turing id=2`). Hover a column header for its sort (`⇅`) and
filter (`▽`) icons: the filter popover writes such a term for that column, so
several columns can be filtered at once; clicking a lit filter icon removes it. Query, sort and scroll position are
kept in the URL (`?q=...&sort=col&dir=desc&row=250`), so a link opens at the
same place. Drag a column header's right edge to resize it, double-click it to fit
the content and again to fit the label. Key columns stay put when scrolling
sideways, edited cells are highlighted, and the focused cell is mirrored in an
editor above the table; Esc reverts that one cell. **Download CSV** streams the whole table, and **Import CSV…** under a
database in the tree loads a CSV file into a table. For a new table, the
header row names the columns and each column gets the narrowest type all of
its values fit (bit, int, bigint, float, date, datetime2, datetimeoffset,
otherwise nvarchar), every column nullable. If the table already exists, the
rows are appended after a confirmation: the header columns must exist in the
table (matched case-insensitively; identity, computed and rowversion columns
are skipped so a downloaded CSV imports back), and values are converted by
SQL Server like grid edits. Empty fields become NULL, a UTF-8 BOM is
skipped, and semicolon or tab delimiters (Excel) are detected from the
header line. Everything runs in one transaction, so a failed import leaves
nothing behind.

Tables with a primary key are editable: change cells, tick rows to delete, or
add rows. Nothing is written until **Save** (or Enter in a cell, Ctrl+Enter in
a multi-line editor), which applies every pending change in one transaction
(all or nothing). Tables without a primary key are
append-only, views are read-only. The Save button and the footer status
highlight unsaved changes, and leaving the page or the table asks first.
The footer shows the build version (`git describe`) and links to Help.

## Layout

```
backend/   Go module: server, auth, SQL API; embeds backend/dist
web/       Vite + React UI; `pnpm build` writes to ../backend/dist
Makefile   make help lists the targets
```

## Run locally

```bash
cp .env.example .env   # fill in the values; make exports them
make dev               # backend on :8080, Vite on :5173 (proxies /api and /auth)
```

`make help` shows every target: `dev`, `build`, `test`, `image`, `run`, `clean`.

## Dev mode (no Entra)

Set `DEV_USER` to skip Entra entirely. Every request runs as that name and
servers are opened with the SQL login in their `SQL_SERVERS` URL. Anyone who
can reach the port is that user, so never set it in production. It listens on
localhost only by default, and it refuses to start if Entra variables
(`TENANT_ID`/`CLIENT_SECRET`) are also set. Logout is a no-op in dev mode.

```bash
make run-sqlserver   # foreground SQL Server 2022 on :1433, sa / Dev_Passw0rd (override with SA_PASSWORD=...); Ctrl-C stops it
# in .env: DEV_USER=dev and SQL_SERVERS='sqlserver://sa:Dev_Passw0rd@localhost:1433?trustservercertificate=true'
make dev
```

The image is amd64-only and segfaults under QEMU emulation, so on Apple
Silicon it needs Rosetta: enable it in Docker Desktop, or for podman put
`rosetta = true` under `[machine]` in `~/.config/containers/containers.conf`
and recreate the machine with `podman machine rm -f && podman machine init -m 4096 --now`
(SQL Server also refuses to start with less than 2 GB, and the podman default VM has exactly that).
The Entra token path is the one thing dev mode does not exercise.

## Build

```bash
make test    # go vet, go test, tsc
make build   # ./mssql-webui with the UI embedded; version from git describe (VERSION=... to override)
```

## Container image (Docker or Podman)

Prebuilt multi-arch images (amd64, arm64) are published to GitHub Container
Registry on every push to `main` (tagged `main`) and on `v*` release tags:

```bash
podman run --rm -p 8080:8080 --env-file .env ghcr.io/pspoerri/mssql-webui:main
```

To build locally instead:

```bash
make image                    # docker build -t mssql-webui .
make image CONTAINER=podman   # or set CONTAINER=podman in .env
make run                      # runs the image on :8080 with --env-file .env
```

Without make:

```bash
podman build -t mssql-webui .
podman run --rm -p 8080:8080 --env-file .env mssql-webui
```

On macOS, Podman needs a VM first: `podman machine init && podman machine start`.
The Dockerfile uses fully qualified image names so Podman never asks which
registry to pull from.

## Limits (by design, easy to add)

Sessions live in memory (single instance) and end after 30 minutes without a request (a visible tab pings every 5 minutes) or 12 hours after login, closing their SQL connections. Edits are last-write-wins. Cell
values are sent as strings and converted by SQL Server. Clearing a nullable
cell writes NULL. The console returns only the first result set. CSV export
writes NULL as an empty field.

Values are rendered the same way in the grid, the console, CSV and text search,
and can be typed back in that form: dates as `2024-01-01 00:13:00`, `1970-01-15`,
`00:10:02`, `2026-09-04 13:32:46.7248911 +00:00`; bit as `true`/`false`; money
with four decimals; decimal and bigint as exact strings. Binary, rowversion,
geometry/geography, hierarchyid and sql_variant columns are shown as base64 and
are read-only; `col=value` on a binary column takes base64. Free-text search
turns each column into text (a table scan; floats with 6 significant digits),
`col=value` compares natively and can use an index.
