package mysql

import (
	"context"
	"fmt"
	"strings"
)

// CheckPrivileges verifies that the monitor user has the privileges the
// collector's queries depend on (README "Required database privileges"):
// REPLICA MONITOR / REPLICATION CLIENT for SHOW REPLICA STATUS, and PROCESS
// to see other users' threads (binlog dump threads) in the processlist.
//
// It is meant to run once at startup, after the database is reachable.
// Failure is not fatal — the endpoints keep serving 503 with the same
// remediation hints — but it is logged loudly, because both missing-
// privilege modes otherwise look like a healthy server whose node is merely
// out of rotation.
func (c *Collector) CheckPrivileges(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	// Probe the real statement rather than parsing privilege names: this
	// exercises the exact code path that fails without REPLICA MONITOR /
	// REPLICATION CLIENT, and reuses the SHOW SLAVE STATUS fallback for
	// old MariaDB. A (nil, nil) result — node not configured as a replica —
	// means the privilege is fine.
	if _, err := c.replicaStatus(ctx); err != nil {
		return fmt.Errorf("replica status: %w", wrapReplicaStatusErr(err))
	}
	return c.checkProcessPrivilege(ctx)
}

// checkProcessPrivilege inspects SHOW GRANTS for the global PROCESS
// privilege (or ALL PRIVILEGES on *.*, which includes it).
func (c *Collector) checkProcessPrivilege(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		return fmt.Errorf("show grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return fmt.Errorf("scan grants: %w", err)
		}
		if hasProcessGrant(grant) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("show grants: %w", err)
	}
	return fmt.Errorf("monitor user lacks the PROCESS privilege: the connected-replica count will always be 0; grant with \"GRANT PROCESS ON *.* TO ...\" (see README: Required database privileges)")
}

// hasProcessGrant reports whether one SHOW GRANTS row grants the global
// PROCESS privilege. PROCESS is global-only, so it must be part of a
// privilege list granted ON *.*: either listed explicitly or via
// ALL PRIVILEGES. Rows granting privileges on single databases/tables (or
// role memberships, which have no ON clause) never grant PROCESS.
func hasProcessGrant(grant string) bool {
	g := strings.ToUpper(grant)
	object, ok := strings.CutPrefix(g, "GRANT ")
	if !ok {
		return false
	}
	privs, on, ok := strings.Cut(object, " ON ")
	if !ok {
		return false
	}
	// The remainder is the object ("*.*", "`db`.*", ...) followed by the
	// "TO user" clause; only the object's first token matters here.
	if object, _, _ = strings.Cut(strings.TrimSpace(on), " "); object != "*.*" {
		return false
	}
	for p := range strings.SplitSeq(privs, ",") {
		switch strings.TrimSpace(p) {
		case "PROCESS", "ALL", "ALL PRIVILEGES":
			return true
		}
	}
	return false
}

// wrapReplicaStatusErr annotates an access-denied error from SHOW REPLICA
// STATUS with the required-privilege remediation so the hint travels with
// every 503 response and log line; other errors pass through unchanged.
func wrapReplicaStatusErr(err error) error {
	if isMySQLError(err, 1227) { // ER_SPECIFIC_ACCESS_DENIED_ERROR
		return fmt.Errorf("monitor user cannot read replication status; needs REPLICA MONITOR (MySQL 8.0.22+/MariaDB 10.5+) or REPLICATION CLIENT (older servers): %w", err)
	}
	return err
}
