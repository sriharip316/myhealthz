package mysql

import (
	"database/sql"
	"testing"
)

func TestParseReplicaStatusMariaDBColumns(t *testing.T) {
	cols := []string{
		"Slave_IO_State", "Master_Host", "Master_User", "Slave_IO_Running",
		"Slave_SQL_Running", "Seconds_Behind_Master",
	}
	vals := []sql.RawBytes{
		[]byte("Waiting for master to send event"), []byte("db1"),
		[]byte("repl"), []byte("Yes"), []byte("Yes"), []byte("0"),
	}
	rs := parseReplicaStatus(cols, vals)
	if rs.SourceHost != "db1" || rs.IORunning != "Yes" || rs.SQLRunning != "Yes" {
		t.Fatalf("got %+v", rs)
	}
	if rs.SecondsBehind == nil || *rs.SecondsBehind != 0 {
		t.Fatalf("SecondsBehind = %v, want 0", rs.SecondsBehind)
	}
}

func TestParseReplicaStatusMySQLColumns(t *testing.T) {
	cols := []string{
		"Replica_IO_State", "Source_Host", "Source_User", "Replica_IO_Running",
		"Replica_SQL_Running", "Seconds_Behind_Source",
	}
	vals := []sql.RawBytes{
		[]byte("Connecting"), []byte("db1"), []byte("repl"),
		[]byte("Connecting"), []byte("Yes"), nil, // NULL lag
	}
	rs := parseReplicaStatus(cols, vals)
	if rs.SourceHost != "db1" || rs.IORunning != "Connecting" || rs.SQLRunning != "Yes" {
		t.Fatalf("got %+v", rs)
	}
	if rs.SecondsBehind != nil {
		t.Fatalf("SecondsBehind = %v, want nil for NULL", *rs.SecondsBehind)
	}
}

func TestParseReplicaStatusCaseInsensitiveLookup(t *testing.T) {
	// Drivers may return column names with varying case.
	cols := []string{"MASTER_HOST", "SLAVE_IO_RUNNING", "SLAVE_SQL_RUNNING"}
	vals := []sql.RawBytes{[]byte("db2"), []byte("No"), []byte("Yes")}
	rs := parseReplicaStatus(cols, vals)
	if rs.SourceHost != "db2" || rs.IORunning != "No" || rs.SQLRunning != "Yes" {
		t.Fatalf("got %+v", rs)
	}
}
