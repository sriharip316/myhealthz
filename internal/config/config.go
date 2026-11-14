// Package config defines the myhealthz command-line configuration.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds every runtime setting for myhealthz.
type Config struct {
	Version        bool
	ListenAddr     string
	Host           string
	Port           int
	Socket         string
	User           string
	Password       string
	passwordFile   string
	MinReplicas    int
	MaxLagSeconds  int
	ConnectTimeout time.Duration
	QueryTimeout   time.Duration
	TLSMode        string // false | true | skip-verify
	TLSCAFile      string
	TLSServerName  string
	TLSCertFile    string
	TLSKeyFile     string
}

// Parse builds a Config from command-line arguments.
func Parse(args []string) (*Config, error) {
	cfg := &Config{}
	fs := flag.NewFlagSet("myhealthz", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", ":8080", "HTTP listen address")
	fs.StringVar(&cfg.Host, "host", "", "MySQL/MariaDB host (TCP); overrides auto-discovery")
	fs.IntVar(&cfg.Port, "port", 3306, "MySQL/MariaDB TCP port")
	fs.StringVar(&cfg.Socket, "socket", "", "Unix socket path; overrides -host/-port and auto-discovery")
	fs.StringVar(&cfg.User, "user", "root", "database user")
	fs.StringVar(&cfg.Password, "password", "", "database password")
	fs.StringVar(&cfg.passwordFile, "password-file", "", "read database password from file")
	fs.IntVar(&cfg.MinReplicas, "min-replicas", 1, "minimum connected replicas for /health/primary to report healthy")
	fs.IntVar(&cfg.MaxLagSeconds, "max-lag", -1, "max replication lag in seconds for /health/replica (-1 disables the check)")
	fs.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 3*time.Second, "dial timeout")
	fs.DurationVar(&cfg.QueryTimeout, "query-timeout", 2*time.Second, "per-check query timeout")
	fs.StringVar(&cfg.TLSMode, "tls", "false", "TLS mode for TCP connections: false | true | skip-verify")
	fs.StringVar(&cfg.TLSCAFile, "tls-ca-file", "", "CA bundle used to verify the server certificate (requires -tls)")
	fs.StringVar(&cfg.TLSServerName, "tls-server-name", "", "expected server name in the server certificate (requires -tls)")
	fs.StringVar(&cfg.TLSCertFile, "tls-cert-file", "", "client certificate for mutual TLS (requires -tls)")
	fs.StringVar(&cfg.TLSKeyFile, "tls-key-file", "", "client key for mutual TLS (requires -tls)")
	fs.BoolVar(&cfg.Version, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.TLSMode {
	case "false", "true", "skip-verify":
	default:
		return fmt.Errorf("invalid -tls value %q: must be false, true or skip-verify", c.TLSMode)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid -port %d", c.Port)
	}
	if c.MinReplicas < 0 {
		return fmt.Errorf("-min-replicas must be >= 0, got %d", c.MinReplicas)
	}
	if c.ConnectTimeout <= 0 || c.QueryTimeout <= 0 {
		return fmt.Errorf("-connect-timeout and -query-timeout must be positive")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("-tls-cert-file and -tls-key-file must be used together")
	}
	if c.TLSMode == "false" {
		for name, val := range map[string]string{
			"-tls-ca-file": c.TLSCAFile, "-tls-server-name": c.TLSServerName,
			"-tls-cert-file": c.TLSCertFile, "-tls-key-file": c.TLSKeyFile,
		} {
			if val != "" {
				return fmt.Errorf("%s requires -tls true or -tls skip-verify", name)
			}
		}
	}
	if c.passwordFile != "" {
		if c.Password != "" {
			return fmt.Errorf("-password and -password-file are mutually exclusive")
		}
		b, err := os.ReadFile(c.passwordFile)
		if err != nil {
			return fmt.Errorf("read -password-file: %w", err)
		}
		c.Password = strings.TrimRight(string(b), "\r\n")
	}
	return nil
}
