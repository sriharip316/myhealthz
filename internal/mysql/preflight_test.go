package mysql

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	gomysql "github.com/go-sql-driver/mysql"
)

// newMockCollector returns a Collector backed by a sqlmock database with
// exact query matching, so expectations must name the SQL verbatim and any
// unexpected query fails the test.
func newMockCollector(t *testing.T) (*Collector, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewCollector(db, time.Second), mock
}

func replicaStatusRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"Slave_IO_State", "Master_Host", "Slave_IO_Running", "Slave_SQL_Running"}).
		AddRow("Waiting for master to send event", "db1", "Yes", "Yes")
}

func grantsRows(grants ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"Grants for myhealthz@%"})
	for _, g := range grants {
		rows.AddRow(g)
	}
	return rows
}

var accessDenied = &gomysql.MySQLError{
	Number:  1227, // ER_SPECIFIC_ACCESS_DENIED_ERROR
	Message: "Access denied; you need (at least one of) the REPLICA MONITOR privilege(s) for this operation",
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestCheckPrivilegesOK(t *testing.T) {
	c, m := newMockCollector(t)
	// Multi-row grants: dynamic privileges land in their own row, so
	// PROCESS in the second row must still be found.
	m.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(replicaStatusRow())
	m.ExpectQuery("SHOW GRANTS FOR CURRENT_USER()").WillReturnRows(grantsRows(
		"GRANT REPLICA MONITOR ON *.* TO `myhealthz`@`%`",
		"GRANT PROCESS ON *.* TO `myhealthz`@`%`",
	))
	if err := c.CheckPrivileges(context.Background()); err != nil {
		t.Fatalf("CheckPrivileges: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCheckPrivilegesNotAReplica(t *testing.T) {
	c, m := newMockCollector(t)
	// No row: the node is not configured as a replica, which is fine.
	m.ExpectQuery("SHOW REPLICA STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Source_Host", "Replica_IO_Running"}))
	m.ExpectQuery("SHOW GRANTS FOR CURRENT_USER()").WillReturnRows(grantsRows(
		"GRANT ALL PRIVILEGES ON *.* TO `myhealthz`@`%` WITH GRANT OPTION",
	))
	if err := c.CheckPrivileges(context.Background()); err != nil {
		t.Fatalf("CheckPrivileges: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCheckPrivilegesReplicaStatusDenied(t *testing.T) {
	c, m := newMockCollector(t)
	// 1227 must NOT trigger the SHOW SLAVE STATUS fallback (that is for
	// 1064 only); the error must carry the remediation hint.
	m.ExpectQuery("SHOW REPLICA STATUS").WillReturnError(accessDenied)
	err := c.CheckPrivileges(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"REPLICA MONITOR", "REPLICATION CLIENT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations (fallback or grants should not have run): %v", err)
	}
}

func TestCheckPrivilegesFallsBackToShowSlave(t *testing.T) {
	c, m := newMockCollector(t)
	m.ExpectQuery("SHOW REPLICA STATUS").
		WillReturnError(&gomysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"})
	m.ExpectQuery("SHOW SLAVE STATUS").WillReturnRows(replicaStatusRow())
	m.ExpectQuery("SHOW GRANTS FOR CURRENT_USER()").WillReturnRows(grantsRows(
		"GRANT REPLICATION CLIENT, PROCESS ON *.* TO `myhealthz`@`%`",
	))
	if err := c.CheckPrivileges(context.Background()); err != nil {
		t.Fatalf("CheckPrivileges: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCheckPrivilegesNoProcess(t *testing.T) {
	c, m := newMockCollector(t)
	m.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(replicaStatusRow())
	m.ExpectQuery("SHOW GRANTS FOR CURRENT_USER()").WillReturnRows(grantsRows(
		"GRANT USAGE ON *.* TO `myhealthz`@`%`",
		"GRANT REPLICA MONITOR ON *.* TO `myhealthz`@`%`",
	))
	err := c.CheckPrivileges(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PROCESS") {
		t.Fatalf("want PROCESS remediation error, got %v", err)
	}
}

func TestHasProcessGrant(t *testing.T) {
	cases := []struct {
		grant string
		want  bool
	}{
		{"GRANT PROCESS ON *.* TO `u`@`%`", true},
		{"GRANT PROCESS, SUPER ON *.* TO `u`@`%`", true},
		{"GRANT SUPER, PROCESS ON *.* TO `u`@`%`", true},
		{"GRANT ALL PRIVILEGES ON *.* TO `u`@`%` WITH GRANT OPTION", true},
		{"GRANT ALL ON *.* TO `u`@`%`", true},
		{"GRANT ALL PRIVILEGES ON `mydb`.* TO `u`@`%`", false},
		{"GRANT SELECT ON `mydb`.`process` TO `u`@`%`", false},
		{"GRANT USAGE ON *.* TO `u`@`%`", false},
		{"GRANT REPLICA MONITOR ON *.* TO `u`@`%`", false},
		{"GRANT PROXY ON `other`@`localhost` TO `u`@`%`", false},
		{"GRANT `monitor_role`@`%` TO `u`@`%`", false},
		{"REVOKE PROCESS ON *.* FROM `u`@`%`", false},
	}
	for _, tc := range cases {
		if got := hasProcessGrant(tc.grant); got != tc.want {
			t.Errorf("hasProcessGrant(%q) = %v, want %v", tc.grant, got, tc.want)
		}
	}
}

func TestFactsReplicaStatusDeniedHint(t *testing.T) {
	c, m := newMockCollector(t)
	m.ExpectQuery("SELECT @@read_only").WillReturnRows(sqlmock.NewRows([]string{"@@read_only"}).AddRow(0))
	m.ExpectQuery("SELECT @@super_read_only").WillReturnRows(sqlmock.NewRows([]string{"@@super_read_only"}).AddRow(0))
	m.ExpectQuery("SHOW REPLICA STATUS").WillReturnError(accessDenied)
	_, err := c.Facts(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "REPLICA MONITOR") {
		t.Errorf("error %q does not carry the remediation hint", err)
	}
}

func TestFactsZeroReplicaWarningConditions(t *testing.T) {
	cases := []struct {
		name       string
		readOnly   int
		replicaRow bool
		connected  int
		wantWarn   bool
	}{
		{"writable primary with no replicas", 0, false, 0, true},
		{"replica connected", 0, false, 1, false},
		{"configured replica", 0, true, 0, false},
		{"read-only", 1, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newMockCollector(t)
			m.ExpectQuery("SELECT @@read_only").WillReturnRows(sqlmock.NewRows([]string{"@@read_only"}).AddRow(tc.readOnly))
			m.ExpectQuery("SELECT @@super_read_only").WillReturnRows(sqlmock.NewRows([]string{"@@super_read_only"}).AddRow(0))
			if tc.replicaRow {
				m.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(replicaStatusRow())
			} else {
				m.ExpectQuery("SHOW REPLICA STATUS").
					WillReturnRows(sqlmock.NewRows([]string{"Source_Host", "Replica_IO_Running"}))
			}
			m.ExpectQuery(countReplicasSQL).
				WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(tc.connected))

			buf := captureLogs(t)
			if _, err := c.Facts(context.Background()); err != nil {
				t.Fatalf("Facts: %v", err)
			}
			if got := strings.Count(buf.String(), "PROCESS privilege"); got != boolToInt(tc.wantWarn) {
				t.Errorf("warning logged %d times, want %d (log: %q)", got, boolToInt(tc.wantWarn), buf.String())
			}
		})
	}
}

func TestFactsZeroReplicaWarningOnce(t *testing.T) {
	c, m := newMockCollector(t)
	for range 2 {
		m.ExpectQuery("SELECT @@read_only").WillReturnRows(sqlmock.NewRows([]string{"@@read_only"}).AddRow(0))
		m.ExpectQuery("SELECT @@super_read_only").WillReturnRows(sqlmock.NewRows([]string{"@@super_read_only"}).AddRow(0))
		m.ExpectQuery("SHOW REPLICA STATUS").
			WillReturnRows(sqlmock.NewRows([]string{"Source_Host", "Replica_IO_Running"}))
		m.ExpectQuery(countReplicasSQL).
			WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	}
	buf := captureLogs(t)
	for range 2 {
		if _, err := c.Facts(context.Background()); err != nil {
			t.Fatalf("Facts: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "PROCESS privilege"); got != 1 {
		t.Errorf("warning logged %d times across 2 calls, want 1", got)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
