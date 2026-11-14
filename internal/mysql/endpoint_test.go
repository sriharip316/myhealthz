package mysql

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestDecideSocketFlagOverrides(t *testing.T) {
	ep := Decide("db1", 3306, "/explicit/mysql.sock", time.Second,
		func(network, addr string, timeout time.Duration) bool {
			t.Errorf("probe should not be called when flags are explicit, got %s:%s", network, addr)
			return false
		}, nil)
	if ep.Network != "unix" || ep.Addr != "/explicit/mysql.sock" {
		t.Fatalf("got %+v, want unix /explicit/mysql.sock", ep)
	}
}

func TestDecideHostFlagOverrides(t *testing.T) {
	ep := Decide("db1", 3307, "", time.Second, nil, nil)
	if ep.Network != "tcp" || ep.Addr != "db1:3307" {
		t.Fatalf("got %+v, want tcp db1:3307", ep)
	}
}

func TestDecideNonDefaultPortAloneIsExplicit(t *testing.T) {
	ep := Decide("", 3307, "", time.Second, nil, nil)
	if ep.Network != "tcp" || ep.Addr != "127.0.0.1:3307" {
		t.Fatalf("got %+v, want tcp 127.0.0.1:3307", ep)
	}
}

// withSocketPaths swaps the well-known socket paths for the duration of a test.
func withSocketPaths(t *testing.T, paths []string) {
	t.Helper()
	orig := defaultSocketPaths
	defaultSocketPaths = paths
	t.Cleanup(func() { defaultSocketPaths = orig })
}

func listenUnix(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mysql.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	return sock
}

func TestDecideDiscoversLiveSocket(t *testing.T) {
	sock := listenUnix(t)
	withSocketPaths(t, []string{sock})

	ep := Decide("", 3306, "", time.Second, DefaultProbe, func(string) string { return "" })
	if ep.Network != "unix" || ep.Addr != sock {
		t.Fatalf("got %+v, want unix %s", ep, sock)
	}
	if ep.Source != "default socket" {
		t.Fatalf("got source %q, want %q", ep.Source, "default socket")
	}
}

func TestDecideEnvSocketIsProbedFirst(t *testing.T) {
	envSock := listenUnix(t)
	withSocketPaths(t, []string{"/nonexistent/mysql.sock"}) // env must win

	ep := Decide("", 3306, "", time.Second, DefaultProbe, func(string) string { return envSock })
	if ep.Network != "unix" || ep.Addr != envSock {
		t.Fatalf("got %+v, want unix %s", ep, envSock)
	}
}

func TestDecideFallsBackToDefaultTCP(t *testing.T) {
	ep := Decide("", 3306, "", time.Second,
		func(network, addr string, timeout time.Duration) bool { return false },
		func(string) string { return "" })
	if ep.Network != "tcp" || ep.Addr != "127.0.0.1:3306" {
		t.Fatalf("got %+v, want tcp 127.0.0.1:3306", ep)
	}
}
