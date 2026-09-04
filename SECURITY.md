# Security model

mssql-webui connects to SQL Server as the signed-in user, not as a service
account. The browser never holds a token: the Go server keeps the user's
Entra ID tokens in memory and hands the access token to every new SQL
connection, so SQL Server sees the person and applies their permissions.

The code behind this is `backend/auth.go` (login, sessions) and
`backend/sql.go` (per-session connection pools).

## Login

```mermaid
sequenceDiagram
    participant B as Browser
    participant S as mssql-webui
    participant E as Entra ID
    B->>S: GET /auth/login
    S-->>B: 302 to Entra, Set-Cookie oauth_state (5 min)
    B->>E: authorize: openid profile offline_access database.windows.net/user_impersonation, state
    Note over E: user signs in, groups claim added
    E-->>B: 302 /auth/callback?code&state
    B->>S: GET /auth/callback?code&state
    Note over S: state = oauth_state cookie? else 400
    S->>E: POST /token: code + client secret
    E-->>S: id_token, refresh_token, access_token
    Note over S: aud = client, tid = tenant, groups contains allowed group, else 403
    Note over S: session in memory: name, email, token source; ends after 30 min idle or 12 h
    S-->>B: 302 /, Set-Cookie sid (HttpOnly, Secure, SameSite=Lax)
```

Tokens stop at the server. A session ends 30 minutes after its last request
or 12 hours after login, whichever comes first, and every SQL connection it
opened is closed with it. The browser only ever receives a random session
id; the code exchange needs `CLIENT_SECRET`, which lives in the server's
environment. The id_token's claims are read without a signature check, which
OIDC Core 3.1.3.7 permits because the token arrives straight from Entra's
token endpoint over TLS.

## Every request after that

```mermaid
sequenceDiagram
    participant B as Browser
    participant S as mssql-webui
    participant E as Entra ID
    participant Q as SQL Server
    B->>S: GET /api/s/{srv}/d/{db}/t/{schema}/{table}/rows, Cookie: sid
    Note over S: sid resolves to the session; idle 30 min or older than 12 h: 401
    Note over S: pool for srv/db in this session, opened on first use
    opt new connection and the cached access token has expired
        S->>E: refresh_token to token endpoint
        E-->>S: access_token
    end
    S->>Q: TDS login, federated auth with access_token
    Note over Q: token verified, connection runs as the user
    Note over Q: paused serverless db: retry every 5 s, up to 2 min
    S->>Q: SELECT with the user's own SQL permissions
    Q-->>S: rows
    S-->>B: JSON
```

The access token is fetched when a connection is opened, not per query. A
pool holds at most 3 connections per database and session and closes idle
ones after 5 minutes, so a refresh happens roughly once an hour per active
database, and only when Entra's token has run out.

## Where each piece lives

| Browser | mssql-webui | Entra ID | SQL Server |
|---|---|---|---|
| `sid` cookie: random 256-bit id, HttpOnly, Secure, SameSite=Lax | sessions in process memory, one per login, dropped after 30 min without a request, 12 h after login, on logout, or at restart; a sweeper runs every minute | issues `id_token`, `access_token` for `database.windows.net`, `refresh_token` | validates the token, maps it to the user's Entra identity |
| `oauth_state` for 5 minutes during login | per session: name, email, token source, one pool per `server/database` | refresh token is good for up to 90 days; the 12 h session cap is what limits a leaked cookie | every user needs a database user; `HAS_DBACCESS` greys out the rest of the tree |
| no token of any kind | `CLIENT_SECRET` from the environment; no SQL credentials in Entra mode | group membership is checked at login only | permissions and audit rows carry the person's name |

## When it does not go through

| Condition | Result |
|---|---|
| state cookie missing or mismatched | callback rejected, 400 |
| not in the allowed group | no session created, 403 access denied |
| refresh token rejected by Entra | 401 session expired; the UI sends the browser back to login |
| no request for 30 min, or session older than 12 h | dropped by the sweeper or on the next request, pools closed, 401; the UI returns to login |
| SQL error | message passed through as 400; it is the user's own permission or syntax problem |
| logout | session and its pools closed, cookie cleared |

## Dev mode

`DEV_USER` skips all of this: one shared session for anyone who reaches the
port, and each server connects with the SQL login in its `SQL_SERVERS` URL.
Never set `DEV_USER` in production. The server refuses to start if it is
combined with `TENANT_ID` or `CLIENT_SECRET`.

## Reporting a vulnerability

Open an issue at https://github.com/pspoerri/mssql-webui/issues, or contact
the maintainer directly if the report should stay private until fixed.
