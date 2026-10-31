# Client connection examples

Every smoke program does the same four things against a PostgreSQL-wire server: connect,
`SELECT 1`, one prepared statement with a parameter, and one `COPY` round trip (FROM STDIN then
TO STDOUT) on a scratch table `suite_smoke` that it creates and drops. They print `OK <client>`
and exit 0, or print the error and exit 1.

Connection settings come from the standard libpq environment variables, so one set of variables
works for all of them. Placeholders only; never put real secrets in the repo.

```
export PGHOST=127.0.0.1 PGPORT=5432 PGUSER=<user> PGPASSWORD=<password> PGDATABASE=<database>
```

| Client | File | Run | Checked |
|---|---|---|---|
| psql | `psql.sh` | `./psql.sh` | ran (psql from the postgres:16 image) |
| Go pgx v5 | `go/main.go` | `go run ./scripts/suite/clients/go` (from the repo root) | ran |
| Python psycopg2 | `python/smoke_psycopg2.py` | `python/run-docker.sh` (runs all three Python smokes in python:3.12-slim) | ran |
| Python psycopg 3 | `python/smoke_psycopg3.py` | `python/run-docker.sh` | ran |
| SQLAlchemy 2 | `python/smoke_sqlalchemy.py` | `python/run-docker.sh` | ran |
| Node pg | `node/smoke.js` | `npm i pg pg-copy-streams && node smoke.js` | syntax only |
| JDBC pgjdbc | `jdbc/Smoke.java` | `jdbc/run-docker.sh` | see the table in docs/25 |
| .NET Npgsql | `dotnet/Smoke.cs` | `dotnet run` in `dotnet/` | syntax only |

## DSN / connection string forms

```
# libpq URI (psql, psycopg2/3, pgx, node pg connectionString)
postgres://<user>:<password>@<host>:5432/<database>?sslmode=disable
postgresql://<user>:<password>@<host>:5432/<database>?sslmode=require&application_name=suite

# libpq key/value (psql, psycopg2/3, pgx)
host=<host> port=5432 user=<user> password=<password> dbname=<database> sslmode=disable

# Go pgx (pgxpool, and extended-protocol default; add default_query_exec_mode=simple_protocol
# to force the simple protocol, or =exec / =describe_exec to avoid prepared statements)
postgres://<user>:<password>@<host>:5432/<database>?sslmode=disable&default_query_exec_mode=simple_protocol

# JDBC (pgjdbc)
jdbc:postgresql://<host>:5432/<database>?user=<user>&password=<password>&sslmode=disable
# prefer the primary of several nodes: list every node, ask for the primary
jdbc:postgresql://<host1>:5432,<host2>:5432,<host3>:5432/<database>?targetServerType=primary&user=<user>&password=<password>
# other values: preferSecondary, secondary, any; loadBalanceHosts=true spreads connections;
# hostRecheckSeconds=10 controls how long a node's role is cached; prepareThreshold=0 disables
# server-side prepared statements; preferQueryMode=simple forces the simple protocol.

# SQLAlchemy (psycopg2 driver, psycopg 3 driver, asyncpg driver)
postgresql+psycopg2://<user>:<password>@<host>:5432/<database>
postgresql+psycopg://<user>:<password>@<host>:5432/<database>
postgresql+asyncpg://<user>:<password>@<host>:5432/<database>

# psycopg3 / libpq multi-host (first host that accepts and is the primary)
postgresql://<user>:<password>@<host1>:5432,<host2>:5432/<database>?target_session_attrs=read-write

# Node pg
postgres://<user>:<password>@<host>:5432/<database>   (or {host, port, user, password, database})

# .NET Npgsql
Host=<host>;Port=5432;Username=<user>;Password=<password>;Database=<database>;SSL Mode=Disable
Host=<host1>,<host2>;Username=<user>;Password=<password>;Database=<database>;Target Session Attributes=primary
```

TLS: `sslmode=require` (libpq, pgx, psycopg, JDBC `sslmode=require`), `SSL Mode=Require` (Npgsql),
`ssl: true` / `ssl: {rejectUnauthorized: false}` (node pg). SCRAM-SHA-256 and `trust` both work with
all of the above; no client setting is needed beyond the password.
