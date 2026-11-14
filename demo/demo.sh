#!/usr/bin/env bash
#
# myhealthz demo: 3 MariaDB nodes + HAProxy on podman, with a guided
# switchover from db1 to db2.
#
#   demo.sh setup       build image, create network, start db1-3 +
#                       myhealthz sidecars + haproxy, wire replication
#   demo.sh status      per-node health + HAProxy routing
#   demo.sh switchover  promote db2, repoint db3, rejoin db1 as replica
#   demo.sh verify      automated E2E assertions (fresh setup + switchover)
#   demo.sh demo        setup + status + switchover + status
#   demo.sh teardown    remove all containers and the network
#
# Topology:
#   db1 (primary) <- db2, db3 (replicas)     HAProxy: :3307 write, :3308 read
#   myhealthz sidecars share each db container's network namespace and
#   connect via localhost:3306 (connection priority step 2).

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd "$SCRIPT_DIR/.." && pwd)

NET="myhealthz-demo"
IMAGE="${MYHEALTHZ_IMAGE:-localhost/myhealthz:dev}"
MARIADB_IMAGE="${MARIADB_IMAGE:-docker.io/library/mariadb:11.8}"
HAPROXY_IMAGE="${HAPROXY_IMAGE:-docker.io/library/haproxy:3.1}"

ROOT_PASSWORD="rootpass"
MON_USER="myhealthz"
MON_PASSWORD="healthpass"
REPL_USER="repl"
REPL_PASSWORD="replpass"

DBS=(db1 db2 db3)
DB_MYSQL_PORT=(13061 13062 13063) # host -> dbN:3306
HTTP_PORT=(18081 18082 18083)     # host -> healthN:8080
WRITE_PORT=13307                  # host -> haproxy:3307 (primary)
READ_PORT=13308                   # host -> haproxy:3308 (replicas)
STATS_PORT=19080                  # host -> haproxy:9000 (stats page)

ALL_CONTAINERS=(haproxy health1 health2 health3 db1 db2 db3)

if [ -t 1 ]; then
  C_BOLD=$'\033[1m'; C_CYAN=$'\033[36m'; C_GREEN=$'\033[32m'
  C_RED=$'\033[31m'; C_DIM=$'\033[2m'; C_RESET=$'\033[0m'
else
  C_BOLD=""; C_CYAN=""; C_GREEN=""; C_RED=""; C_DIM=""; C_RESET=""
fi

say()  { printf '\n%s\n' "${C_CYAN}${C_BOLD}==> $*${C_RESET}"; }
note() { printf '  %s\n' "${C_DIM}$*${C_RESET}"; }
ok()   { printf '  %s\n' "${C_GREEN}OK  $*${C_RESET}"; }
fail() { printf '  %s\n' "${C_RED}FAIL $*${C_RESET}" >&2; }

need() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || { echo "missing required command: $c" >&2; exit 1; }
  done
}

db_index() {
  case "$1" in
    db1) echo 0 ;;
    db2) echo 1 ;;
    db3) echo 2 ;;
    *) echo "unknown node: $1" >&2; return 1 ;;
  esac
}

sql() { # sql <node> <statement>  (via unix socket, as root)
  podman exec -e MYSQL_PWD="$ROOT_PASSWORD" "$1" mariadb -uroot -B -e "$2"
}

sqln() { # sqln <node> <statement>  (no column headers)
  podman exec -e MYSQL_PWD="$ROOT_PASSWORD" "$1" mariadb -uroot -B -N -e "$2"
}

gtid_pos() { sqln "$1" "SELECT @@gtid_binlog_pos"; }

probe() { # probe <node> <path> -> HTTP status code
  local idx
  idx=$(db_index "$1")
  curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${HTTP_PORT[$idx]}/$2"
}

route_name() { # route_name <port> -> hostname serving traffic, or "unreachable"
  MYSQL_PWD="$ROOT_PASSWORD" mariadb -h 127.0.0.1 -P "$1" -u root -B -N \
    -e "SELECT @@hostname" 2>/dev/null || echo "unreachable"
}

qwrite() { MYSQL_PWD="$ROOT_PASSWORD" mariadb -h 127.0.0.1 -P "$WRITE_PORT" -u root -B -N -e "$1"; }
qread()  { MYSQL_PWD="$ROOT_PASSWORD" mariadb -h 127.0.0.1 -P "$READ_PORT"  -u root -B -N -e "$1"; }

wait_route() { # wait_route <port> <expected-host> [timeout-seconds]
  local port=$1 want=$2 timeout=${3:-30}
  local deadline=$((SECONDS + timeout)) got
  while :; do
    got=$(route_name "$port")
    if [ "$got" = "$want" ]; then break; fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "port $port did not route to $want within ${timeout}s (last: $got)"
      return 1
    fi
    sleep 0.5
  done
  note "port $port routes to $want"
}

wait_for() { # wait_for <node> <path> <expected-code> [description] [timeout-seconds]
  local node=$1 path=$2 want=$3 what=${4:-$1 /$2} timeout=${5:-90}
  local deadline=$((SECONDS + timeout)) code
  while :; do
    code=$(probe "$node" "$path")
    if [ "$code" = "$want" ]; then break; fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "timed out waiting for $what (wanted HTTP $want, got $code)"
      return 1
    fi
    sleep 0.5
  done
  note "$what -> HTTP $want"
}

wait_db_ready() { # wait for the final TCP listener (skips the entrypoint's temp server)
  local node=$1 deadline=$((SECONDS + 120))
  until podman exec -e MYSQL_PWD="$ROOT_PASSWORD" "$node" \
      mariadb -h 127.0.0.1 -P 3306 -u root -B -e "SELECT 1" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then fail "$node did not become ready"; return 1; fi
    sleep 1
  done
  note "$node ready"
}

replica_running() { # both replication threads are Yes
  [ "$(sql "$1" 'SHOW REPLICA STATUS\G' | grep -c 'Running: Yes')" -eq 2 ]
}

wait_replica_running() {
  local node=$1 deadline=$((SECONDS + 60))
  until replica_running "$node"; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "$node: replication threads not running"
      sql "$node" 'SHOW REPLICA STATUS\G' || true
      return 1
    fi
    sleep 0.5
  done
  note "$node: replication threads running"
}

wait_synced() { # wait_synced <node> <gtid-pos> [timeout-seconds]
  local node=$1 pos=$2 deadline=$((SECONDS + ${3:-60})) r
  while :; do
    r=$(sqln "$node" "SELECT MASTER_GTID_WAIT('$pos', 5)") || r="-1"
    if [ "$r" = "0" ]; then break; fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "$node: not synced to $pos"
      return 1
    fi
    sleep 0.5
  done
  note "$node: synced to $pos"
}

# Merge two GTID positions; the source position wins on shared domains.
# With gtid_strict_mode, gtid_slave_pos must contain a value for every
# domain in the node's own binlog, so the node keeps its local domains
# while adopting the source's position for replicated ones.
merge_gtid_pos() { # merge_gtid_pos <own-pos> <source-pos>
  local own=$1 src=$2 entry domain merged
  local -A doms=()
  for entry in ${src//,/ }; do
    doms[${entry%%-*}]=$entry
  done
  for entry in ${own//,/ }; do
    domain=${entry%%-*}
    if [ -z "${doms[$domain]:-}" ]; then
      doms[$domain]=$entry
    fi
  done
  for domain in $(printf '%s\n' "${!doms[@]}" | sort -n); do
    merged="${merged:+$merged,}${doms[$domain]}"
  done
  echo "$merged"
}

# Start (or repoint) replication from a source, skipping the source's
# pre-existing local binlog history. All nodes boot with equivalent local
# state (same entrypoint users), so replication links start "from now":
# gtid_slave_pos is pinned to the source's current position (merged with
# the node's own domains), which avoids replaying each node's private
# history across the cluster.
start_replication() { # start_replication <node> <source> [full|host-only]
  local node=$1 source=$2 mode=${3:-full} master_opts pos
  if [ "$mode" = "full" ]; then
    master_opts="MASTER_USER='$REPL_USER', MASTER_PASSWORD='$REPL_PASSWORD',
                 MASTER_CONNECT_RETRY=5,"
  else
    master_opts="" # keep existing credentials when only switching host
  fi
  pos=$(merge_gtid_pos "$(gtid_pos "$node")" "$(gtid_pos "$source")")
  sql "$node" "STOP REPLICA" >/dev/null 2>&1 || true
  sql "$node" "SET GLOBAL gtid_slave_pos='$pos'"
  sql "$node" "CHANGE MASTER TO MASTER_HOST='$source', MASTER_PORT=3306, $master_opts MASTER_USE_GTID=slave_pos;"
  sql "$node" "START REPLICA;"
}

create_users() {
  local node
  for node in "${DBS[@]}"; do
    sql "$node" "CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PASSWORD';
                 GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
                 CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PASSWORD';"
    # MariaDB 10.5+ splits SHOW REPLICA STATUS into REPLICA MONITOR;
    # older MariaDB and MySQL accept REPLICATION CLIENT.
    sql "$node" "GRANT REPLICA MONITOR, PROCESS ON *.* TO '$MON_USER'@'%'" \
      || sql "$node" "GRANT REPLICATION CLIENT, PROCESS ON *.* TO '$MON_USER'@'%'"
  done
  note "replication user '$REPL_USER' and monitor user '$MON_USER' present on all nodes"
}

setup() {
  need podman curl mariadb make

  for c in "${ALL_CONTAINERS[@]}"; do
    if podman container exists "$c" 2>/dev/null; then
      say "Existing demo stack found; removing it first"
      teardown
      break
    fi
  done

  say "Building $IMAGE (skipped if it already exists)"
  podman image exists "$IMAGE" || make -C "$ROOT_DIR" image

  say "Creating podman network $NET"
  podman network exists "$NET" 2>/dev/null || podman network create "$NET"

  local i node sid
  for i in 0 1 2; do
    node=${DBS[$i]}
    sid=$((i + 1))
    say "Starting $node (MariaDB server-id=$sid) + myhealthz sidecar"
    podman run -d --name "$node" --hostname "$node" --network "$NET" \
      -p "${DB_MYSQL_PORT[$i]}:3306" -p "${HTTP_PORT[$i]}:8080" \
      -e MARIADB_ROOT_PASSWORD="$ROOT_PASSWORD" \
      "$MARIADB_IMAGE" \
      --server-id="$sid" --log-bin=binlog --gtid-strict-mode=1 --gtid-domain-id="$sid" >/dev/null
    # Sidecar shares db's network namespace: no -host/-socket flags, so
    # myhealthz falls back to TCP localhost:3306 (priority step 2).
    podman run -d --name "health$((i + 1))" --network "container:$node" \
      "$IMAGE" -listen :8080 -user "$MON_USER" -password "$MON_PASSWORD" >/dev/null
    wait_db_ready "$node"
  done

  say "Creating users identically on every node"
  create_users

  say "Configuring db2 and db3 as read-only replicas of db1"
  sql db2 "SET GLOBAL read_only=1"
  sql db3 "SET GLOBAL read_only=1"
  start_replication db2 db1 full
  start_replication db3 db1 full
  wait_replica_running db2
  wait_replica_running db3
  wait_synced db2 "$(gtid_pos db1)"
  wait_synced db3 "$(gtid_pos db1)"

  say "Starting HAProxy (:$WRITE_PORT write, :$READ_PORT read, :$STATS_PORT stats)"
  podman run -d --name haproxy --network "$NET" \
    -p "$WRITE_PORT:3307" -p "$READ_PORT:3308" -p "$STATS_PORT:9000" \
    -v "$SCRIPT_DIR/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
    "$HAPROXY_IMAGE" >/dev/null

  say "Waiting for db1 /health/primary = 200 (requires >= 1 connected replica)"
  wait_for db1 health/primary 200 "db1 /health/primary"

  say "Writing demo data through the HAProxy write port"
  qwrite "CREATE DATABASE IF NOT EXISTS demo;
          CREATE TABLE IF NOT EXISTS demo.hits (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(64), at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
          INSERT INTO demo.hits (note) VALUES ('written before switchover, via haproxy write port');"

  ok "setup complete: db1 is primary; db2, db3 are replicas"
  note "stats page: http://127.0.0.1:$STATS_PORT   |   next: $0 status"
}

status() {
  need curl
  local i node
  say "Per-node health (HTTP codes + myhealthz /status)"
  for i in 0 1 2; do
    node=${DBS[$i]}
    printf '  %s%-4s%s /health/primary=%s  /health/replica=%s\n' \
      "$C_BOLD" "$node" "$C_RESET" "$(probe "$node" health/primary)" "$(probe "$node" health/replica)"
    curl -s --max-time 3 "http://127.0.0.1:${HTTP_PORT[$i]}/status" | sed 's/^/    /'
  done

  say "HAProxy routing"
  note "write :$WRITE_PORT -> $(route_name "$WRITE_PORT")"
  local r
  for r in 1 2 3; do note "read  :$READ_PORT -> $(route_name "$READ_PORT")"; done
  note "stats: http://127.0.0.1:$STATS_PORT"
}

switchover() {
  need podman curl mariadb

  say "Before: write port :$WRITE_PORT -> $(route_name "$WRITE_PORT")"

  say "Step 1/4: demote db1 (read_only ON -> primary health fails)"
  sql db1 "SET GLOBAL read_only=1"
  sql db1 "SET GLOBAL super_read_only=1" 2>/dev/null \
    || note "super_read_only is MySQL-only; on MariaDB, plain read_only still lets SUPER users write"
  wait_for db1 health/primary 503 "db1 /health/primary"
  note "HAProxy drops db1 from both backends: it is quarantined, not split-brain"

  say "Step 2/4: promote db2 (stop replication, catch up, make writable)"
  sql db2 "STOP REPLICA"
  wait_synced db2 "$(gtid_pos db1)"
  sql db2 "SET GLOBAL read_only=0"
  sql db2 "RESET REPLICA ALL" 2>/dev/null || sql db2 "RESET SLAVE ALL"
  note "db2 /health/primary -> $(probe db2 health/primary) (needs >=1 connected replica first)"
  note "write port has NO healthy backend in this window: clients wait, nothing splits"

  say "Step 3/4: repoint db3 from db1 to db2"
  wait_synced db3 "$(gtid_pos db1)" # finish applying db1's events
  start_replication db3 db2 host-only
  wait_replica_running db3
  wait_synced db3 "$(gtid_pos db2)"

  say "Step 4/4: db2 becomes primary once a replica is connected; rejoin db1"
  wait_for db2 health/primary 200 "db2 /health/primary"
  start_replication db1 db2 full
  wait_replica_running db1
  wait_synced db1 "$(gtid_pos db2)"

  qwrite "INSERT INTO demo.hits (note) VALUES ('written after switchover, via haproxy write port');"
  note "write :$WRITE_PORT -> $(route_name "$WRITE_PORT") (was db1)"
  note "reads :$READ_PORT -> $(route_name "$READ_PORT") (db1 + db3, both replicas now)"
  ok "switchover complete: db2 primary, db1 + db3 replicas"
}

verify() {
  need podman curl mariadb
  local failures=0

  assert_code() { # assert_code <description> <actual> <expected>
    if [ "$2" = "$3" ]; then
      ok "$1: $2"
    else
      fail "$1: got $2, want $3"
      failures=$((failures + 1))
    fi
  }
  assert_host() { # assert_host <description> <actual> <allowed...>
    local desc=$1 actual=$2 allowed match=0
    shift 2
    for allowed in "$@"; do
      if [ "$actual" = "$allowed" ]; then match=1; fi
    done
    if [ "$match" = 1 ]; then ok "$desc: $actual"; else fail "$desc: got $actual, want one of: $*"; failures=$((failures + 1)); fi
  }

  say "verify: fresh E2E run"
  setup

  say "verify: initial topology (db1 primary, db2/db3 replicas)"
  wait_route "$WRITE_PORT" db1 # let HAProxy finish its first check cycles
  assert_code "db1 /health/primary" "$(probe db1 health/primary)" 200
  assert_code "db1 /health/replica" "$(probe db1 health/replica)" 503
  assert_code "db2 /health/primary" "$(probe db2 health/primary)" 503
  assert_code "db2 /health/replica" "$(probe db2 health/replica)" 200
  assert_code "db3 /health/primary" "$(probe db3 health/primary)" 503
  assert_code "db3 /health/replica" "$(probe db3 health/replica)" 200
  assert_host "write port routes to" "$(route_name "$WRITE_PORT")" db1
  assert_host "read port routes to" "$(route_name "$READ_PORT")" db2 db3
  assert_host "read port routes to" "$(route_name "$READ_PORT")" db2 db3

  say "verify: switchover db1 -> db2"
  switchover

  say "verify: new topology (db2 primary, db1/db3 replicas)"
  wait_route "$WRITE_PORT" db2
  assert_code "db1 /health/primary" "$(probe db1 health/primary)" 503
  assert_code "db1 /health/replica" "$(probe db1 health/replica)" 200
  assert_code "db2 /health/primary" "$(probe db2 health/primary)" 200
  assert_code "db2 /health/replica" "$(probe db2 health/replica)" 503
  assert_code "db3 /health/primary" "$(probe db3 health/primary)" 503
  assert_code "db3 /health/replica" "$(probe db3 health/replica)" 200
  assert_host "write port routes to" "$(route_name "$WRITE_PORT")" db2
  assert_host "read port routes to" "$(route_name "$READ_PORT")" db1 db3

  local rows
  rows=$(qwrite "SELECT COUNT(*) FROM demo.hits WHERE note LIKE 'written after switchover%'")
  assert_code "post-switchover write visible via write port" "$rows" 1

  if [ "$failures" -eq 0 ]; then
    ok "ALL CHECKS PASSED"
  else
    fail "$failures check(s) failed"
    exit 1
  fi
}

demo() {
  setup
  status
  if [ -t 0 ]; then
    printf '\n%s' "${C_BOLD}Press Enter to run the switchover...${C_RESET}"
    read -r
  fi
  switchover
  status
}

any_left() {
  local c
  for c in "${ALL_CONTAINERS[@]}"; do
    if podman container exists "$c" 2>/dev/null; then return 0; fi
  done
  return 1
}

teardown() {
  say "Removing demo containers and network"
  # rootless netns teardown can fail transiently while still succeeding;
  # retry until the containers are really gone.
  local attempt
  for attempt in 1 2 3 4 5; do
    if podman rm -f --ignore "${ALL_CONTAINERS[@]}" >/dev/null 2>&1; then break; fi
    if ! any_left; then break; fi
    note "cleanup retry $attempt/5"
    sleep 2
  done
  if any_left; then
    fail "some containers could not be removed; run teardown again"
    exit 1
  fi
  podman network rm "$NET" >/dev/null 2>&1 || true
  ok "teardown complete"
}

case "${1:-}" in
  setup | status | switchover | verify | demo | teardown)
    "$@"
    ;;
  *)
    grep '^#   ' "$0" | sed 's/^#   //; s/^#//'
    exit 1
    ;;
esac
