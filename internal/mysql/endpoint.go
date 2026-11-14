// Package mysql handles connecting to MySQL/MariaDB and collecting
// replication facts from the server.
package mysql

import (
	"net"
	"os"
	"strconv"
	"time"

	"github.com/sriharip316/myhealthz/internal/config"
)

// Endpoint is the server a Collector talks to.
type Endpoint struct {
	Network string // "unix" or "tcp"
	Addr    string // socket path or host:port
	Source  string // how the endpoint was chosen, for startup logs
}

func (e Endpoint) String() string {
	if e.Network == "unix" {
		return "unix://" + e.Addr
	}
	return "tcp://" + e.Addr
}

// Prober reports whether an address accepts connections.
type Prober func(network, addr string, timeout time.Duration) bool

// DefaultProbe dials the address once with the given timeout.
func DefaultProbe(network, addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// defaultSocketPaths lists well-known MySQL/MariaDB socket locations,
// in probe order.
var defaultSocketPaths = []string{
	"/var/run/mysqld/mysqld.sock",
	"/run/mysqld/mysqld.sock",
	"/run/mysql/mysql.sock",
	"/tmp/mysql.sock",
	"/var/lib/mysql/mysql.sock",
}

func socketCandidates(getenv func(string) string) []string {
	candidates := []string{}
	if p := getenv("MYSQL_UNIX_PORT"); p != "" {
		candidates = append(candidates, p)
	}
	return append(candidates, defaultSocketPaths...)
}

// Decide picks the server endpoint. Explicit flags win: -socket is used as-is,
// and a non-empty -host (or a non-default -port) produces a TCP endpoint.
// Otherwise the first live default unix socket is used, falling back to TCP
// on localhost:3306.
func Decide(host string, port int, socket string, dialTimeout time.Duration, probe Prober, getenv func(string) string) Endpoint {
	if socket != "" {
		return Endpoint{Network: "unix", Addr: socket, Source: "flag -socket"}
	}
	if host != "" || port != 3306 {
		if host == "" {
			host = "127.0.0.1"
		}
		return Endpoint{Network: "tcp", Addr: net.JoinHostPort(host, strconv.Itoa(port)), Source: "flags -host/-port"}
	}
	if probe == nil {
		probe = DefaultProbe
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, p := range socketCandidates(getenv) {
		if probe("unix", p, dialTimeout) {
			return Endpoint{Network: "unix", Addr: p, Source: "default socket"}
		}
	}
	return Endpoint{Network: "tcp", Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Source: "default tcp localhost:3306"}
}

// ResolveEndpoint applies the connection priority to a Config.
func ResolveEndpoint(cfg *config.Config) Endpoint {
	return Decide(cfg.Host, cfg.Port, cfg.Socket, cfg.ConnectTimeout, DefaultProbe, os.Getenv)
}
