# mssql-webui

Browse and edit MS SQL databases in the browser with your own Entra ID
identity. One Go binary serves the API and the React UI; SQL connections
use the logged-in user's Entra access token, so SQL permissions apply
per user.

## Entra app registration

1. Entra ID → App registrations → New. Platform **Web**, redirect URI
   `https://<host>/auth/callback` (for local dev also
   `http://localhost:5173/auth/callback`).
2. Certificates & secrets → new client secret → `CLIENT_SECRET`.
3. API permissions → Add → APIs my organization uses → **Azure SQL Database**
   → Delegated → `user_impersonation`. Grant admin consent if your tenant
   requires it.
4. Token configuration → Add groups claim → **Security groups**, and tick
   **Groups assigned to the application** so the claim never overflows.
5. Enterprise applications → this app → Users and groups → assign the group
   you will put in `ALLOWED_GROUP_ID`. Optionally set Properties →
   **Assignment required** so Entra refuses everyone else.

Every server must accept Entra logins, and each user needs a database user
(`CREATE USER [name@tenant] FROM EXTERNAL PROVIDER`) or group login on the
databases they should see. Listing databases queries `sys.databases` on
`master`, so users need access to `master` too; otherwise the server shows
an error and an empty list.

## Configuration

| Env var | Meaning |
|---|---|
| `TENANT_ID` | Entra tenant ID (the directory GUID, not the domain name) |
| `CLIENT_ID`, `CLIENT_SECRET` | from the app registration |
| `REDIRECT_URL` | `https://<host>/auth/callback` |
| `ALLOWED_GROUP_ID` | object ID of the Entra group allowed in |
| `SQL_SERVERS` | comma-separated go-mssqldb URLs — without credentials in Entra mode, with a SQL login in dev mode, e.g. `sqlserver://sql1.internal:1433?encrypt=true,sqlserver://sql2:1433?trustservercertificate=true` |
| `LISTEN_ADDR` | default `:8080` |
| `DEV_USER` | skip Entra; run as this user with SQL logins from `SQL_SERVERS`. Dev only. |

The process needs TCP reachability to every server (peered VNet, private
endpoint, or VPN).

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
docker run -d --name sql -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='Dev_Passw0rd' \
  -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest
# in .env: DEV_USER=dev and SQL_SERVERS='sqlserver://sa:Dev_Passw0rd@localhost:1433?trustservercertificate=true'
make dev
```

On Apple Silicon add `--platform linux/amd64` and enable Rosetta in Docker
Desktop. The Entra token path is the one thing dev mode does not exercise.

## Build

```bash
make test    # go vet, go test, tsc
make build   # ./mssql-webui with the UI embedded
```

## Container image (Docker or Podman)

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

Sessions live in memory (single instance). Edits are last-write-wins. Cell
values are sent as strings and converted by SQL Server. Clearing a nullable
cell writes NULL. The console returns only the first result set.
