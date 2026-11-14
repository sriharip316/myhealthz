package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// Facts is a point-in-time snapshot of the replication state of a server.
type Facts struct {
	ReadOnly               bool   `json:"read_only"`
	SuperReadOnly          bool   `json:"super_read_only"`
	SuperReadOnlySupported bool   `json:"super_read_only_supported"`
	ReplicaConfigured      bool   `json:"replica_configured"`
	SourceHost             string `json:"source_host,omitempty"`
	IORunning              string `json:"replica_io_running,omitempty"`
	SQLRunning             string `json:"replica_sql_running,omitempty"`
	SecondsBehindSource    *int64 `json:"seconds_behind_source,omitempty"`
	ConnectedReplicas      int    `json:"connected_replicas"`
}

// ReplicaStatus holds the replication fields myhealthz cares about, parsed
// from SHOW REPLICA STATUS / SHOW SLAVE STATUS.
type ReplicaStatus struct {
	SourceHost    string
	IORunning     string
	SQLRunning    string
	SecondsBehind *int64
}

const (
	stmtShowReplica = "SHOW REPLICA STATUS"
	stmtShowSlave   = "SHOW SLAVE STATUS"

	// One dump thread per connected replica, on both MySQL and MariaDB.
	countReplicasSQL = "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%'"
)

// Collector gathers Facts from a server on demand.
type Collector struct {
	db                *sql.DB
	queryTimeout      time.Duration
	replicaStmt       atomic.Value // string
	superROUnknownLog atomic.Bool
}

// NewCollector wraps a *sql.DB opened with Open.
func NewCollector(db *sql.DB, queryTimeout time.Duration) *Collector {
	c := &Collector{db: db, queryTimeout: queryTimeout}
	c.replicaStmt.Store(stmtShowReplica)
	return c
}

// Facts runs every check query and returns the snapshot, or the first error.
func (c *Collector) Facts(ctx context.Context) (*Facts, error) {
	ctx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	f := &Facts{}
	if err := c.collectReadOnly(ctx, f); err != nil {
		return nil, fmt.Errorf("read_only: %w", err)
	}
	rs, err := c.replicaStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("replica status: %w", err)
	}
	if rs != nil {
		f.ReplicaConfigured = true
		f.SourceHost = rs.SourceHost
		f.IORunning = rs.IORunning
		f.SQLRunning = rs.SQLRunning
		f.SecondsBehindSource = rs.SecondsBehind
	}
	if err := c.db.QueryRowContext(ctx, countReplicasSQL).Scan(&f.ConnectedReplicas); err != nil {
		return nil, fmt.Errorf("connected replicas: %w", err)
	}
	return f, nil
}

func (c *Collector) collectReadOnly(ctx context.Context, f *Facts) error {
	var ro int
	if err := c.db.QueryRowContext(ctx, "SELECT @@read_only").Scan(&ro); err != nil {
		return err
	}
	f.ReadOnly = ro != 0

	// MariaDB has no super_read_only; its read_only already blocks SUPER
	// users, so treating it as unsupported is safe.
	var sro int
	err := c.db.QueryRowContext(ctx, "SELECT @@super_read_only").Scan(&sro)
	switch {
	case err == nil:
		f.SuperReadOnlySupported = true
		f.SuperReadOnly = sro != 0
	case isMySQLError(err, 1193): // ER_UNKNOWN_SYSTEM_VARIABLE
		if c.superROUnknownLog.CompareAndSwap(false, true) {
			log.Printf("myhealthz: server does not support super_read_only (MariaDB); relying on read_only")
		}
	default:
		return err
	}
	return nil
}

// replicaStatus returns nil (and no error) when the server has no replica
// configuration at all.
func (c *Collector) replicaStatus(ctx context.Context) (*ReplicaStatus, error) {
	stmt := c.replicaStmt.Load().(string)
	rows, err := c.db.QueryContext(ctx, stmt)
	if err != nil {
		if isMySQLError(err, 1064) && stmt == stmtShowReplica { // ER_PARSE_ERROR
			c.replicaStmt.Store(stmtShowSlave)
			log.Printf("myhealthz: SHOW REPLICA STATUS unsupported; falling back to SHOW SLAVE STATUS")
			rows, err = c.db.QueryContext(ctx, stmtShowSlave)
		}
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	raw := make([]sql.RawBytes, len(cols))
	scan := make([]any, len(raw))
	for i := range raw {
		scan[i] = &raw[i]
	}
	if !rows.Next() {
		return nil, rows.Err() // no row: not configured as a replica
	}
	if err := rows.Scan(scan...); err != nil {
		return nil, err
	}
	return parseReplicaStatus(cols, raw), nil
}

// parseReplicaStatus maps a result row to ReplicaStatus. MariaDB keeps the
// Master_*/Slave_* column names in SHOW REPLICA STATUS while MySQL 8.0.22+
// renamed them to Source_*/Replica_*, so both families are accepted and
// columns are looked up by name, never by position.
func parseReplicaStatus(cols []string, vals []sql.RawBytes) *ReplicaStatus {
	m := make(map[string]string, len(cols))
	for i, col := range cols {
		if vals[i] == nil {
			continue
		}
		m[strings.ToLower(col)] = string(vals[i])
	}
	pick := func(names ...string) (string, bool) {
		for _, n := range names {
			if v, ok := m[n]; ok {
				return v, true
			}
		}
		return "", false
	}
	rs := &ReplicaStatus{}
	rs.SourceHost, _ = pick("master_host", "source_host")
	rs.IORunning, _ = pick("slave_io_running", "replica_io_running")
	rs.SQLRunning, _ = pick("slave_sql_running", "replica_sql_running")
	if v, ok := pick("seconds_behind_master", "seconds_behind_source"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rs.SecondsBehind = &n
		}
	}
	return rs
}

func isMySQLError(err error, number uint16) bool {
	var myErr *gomysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == number
}
