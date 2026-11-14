# myhealthz

HTTP health check server for **MySQL** and **MariaDB** that answers one
question for load balancers: _is this server the **primary** or a
**replica** right now?_

Run it next to a database (sidecar or plain process), point HAProxy /
kube-proxy / any HTTP prober at it, and route writes to primaries and
reads to replicas — safely, even across switchover.

## Endpoints

| Endpoint              | 200 when                                    | 503 when                       |
| --------------------- | ------------------------------------------- | ------------------------------ |
| `GET /health/primary` | the server is the primary (see below)       | anything else, incl. DB errors |
| `GET /health/replica` | the server is a healthy replica (see below) | anything else, incl. DB errors |
| `GET /status`         | always 200: full facts + decision + reasons | —                              |
| `GET /healthz`        | always 200 (process liveness)               | —                              |

Responses are JSON and include the reasons behind the verdict:

```json
{
  "ok": false,
  "endpoint": "primary",
  "role": "none",
  "reasons": ["connected replicas 0 < required 1"],
  "facts": {
    "read_only": false,
    "super_read_only": false,
    "replica_configured": false,
    "connected_replicas": 0
  }
}
```

Role transitions are logged, so a switchover is visible in `podman logs`.

## Roles

**Primary** — all of:

- `read_only` / `super_read_only` is **off**
- not replicating from anyone (no `SHOW REPLICA STATUS` row)
- at least `-min-replicas` replicas connected to it (default 1; counted as
  `Binlog Dump%` threads in the processlist)

**Replica** — all of:

- `read_only` / `super_read_only` is **on**
- replication configured **and both threads running** (`IO` + `SQL`)
- within `-max-lag` seconds of the source (check disabled by default)

Anything else — a demoted old primary, a promoted node whose replicas
haven't reconnected yet, a replica with a stopped thread — is **neither**
(503 on both endpoints). That is deliberate: during a switchover the
write backend should have _no_ healthy server rather than two.

## Connecting to the database

Priority (explicit flags win; discovery only runs when no endpoint flag
is given):

1. **Default unix socket** — the first live socket among `$MYSQL_UNIX_PORT`,
   `/var/run/mysqld/mysqld.sock`, `/run/mysqld/mysqld.sock`,
   `/run/mysql/mysql.sock`, `/tmp/mysql.sock`, `/var/lib/mysql/mysql.sock`
2. **TCP localhost:3306**
3. **CLI args** — `-socket`, or `-host`/`-port`
4. **TLS (optional, TCP only)** — `-tls true|skip-verify`, `-tls-ca-file`,
   `-tls-server-name`, `-tls-cert-file`/`-tls-key-file` (mutual TLS)

The chosen endpoint and its source are logged at startup.

### Flags

```
-listen string        HTTP listen address (default ":8080")
-host string          MySQL/MariaDB host (TCP); overrides auto-discovery
-port int             MySQL/MariaDB TCP port (default 3306)
-socket string        Unix socket path; overrides -host/-port and auto-discovery
-user string          database user (default "root")
-password string      database password
-password-file string read database password from file
-min-replicas int     minimum connected replicas for /health/primary (default 1)
-max-lag int          max replication lag in seconds for /health/replica (-1 = off)
-connect-timeout      dial timeout (default 3s)
-query-timeout        per-check query timeout (default 2s)
-tls string           false | true | skip-verify (default "false")
-tls-ca-file string   CA bundle to verify the server certificate
-tls-server-name ...  expected server name in the certificate
-tls-cert-file / -tls-key-file   client certificate for mutual TLS
```

### Required database privileges

```sql
CREATE USER 'myhealthz'@'%' IDENTIFIED BY '...';
-- MariaDB 10.5+: REPLICA MONITOR covers SHOW REPLICA STATUS.
-- MySQL and older MariaDB: use REPLICATION CLIENT instead.
GRANT REPLICA MONITOR, PROCESS ON *.* TO 'myhealthz'@'%';
```

(`PROCESS` is needed to count connected replicas in the processlist.
Grant these before starting myhealthz — MariaDB evaluates some
privileges per connection, so grants may not apply to already-open
connections.)

## MySQL vs MariaDB compatibility

- `super_read_only` is MySQL-only; on MariaDB (where plain `read_only`
  already blocks privileged users) the query failure is detected once,
  logged, and treated as off.
- `SHOW REPLICA STATUS` is tried first and falls back to
  `SHOW SLAVE STATUS` on a syntax error (old servers).
- Replica status columns are looked up **by name**, accepting both the
  MariaDB (`Master_Host`, `Slave_IO_Running`, ...) and MySQL 8.0.22+
  (`Source_Host`, `Replica_IO_Running`, ...) families.

## Demo / E2E: 3x MariaDB + HAProxy on podman

The demo runs three MariaDB 11 nodes with GTID replication, one
`myhealthz` sidecar per node (sharing the node's network namespace, so it
uses connection priority step 2 — TCP localhost:3306), and HAProxy with
an HTTP-check write backend (`/health/primary`) and read backend
(`/health/replica`):

```
                       ┌─────────┐  :13307 write ─────> db1 (primary)
 clients ──> haproxy ──┤  stats  │  :13308 read  ──┐
                       └─────────┘                 ├──> db2 (replica)
                                                   └──> db3 (replica)
```

```console
$ make demo          # setup + status + guided switchover + status
$ make verify        # automated E2E assertions (exit code nonzero on failure)
$ make status        # observe any time
$ make teardown
```

`demo/demo.sh switchover` walks through a safe switchover db1 → db2:
demote db1 (`read_only=1`), promote db2 (catch up, `read_only=0`,
`RESET REPLICA ALL`), watch the write backend go dark until db3 repoints
to db2 and db2's `/health/primary` turns 200, then rejoin db1 as a
replica. Ports: `1306N` dbN MySQL, `1808N` sidecar HTTP, `13307`/`13308`
write/read, `19080` HAProxy stats.

## Build

```console
$ make build         # bin/myhealthz (static, CGO off)
$ make image         # localhost/myhealthz:dev (distroless)
$ make test vet
```
