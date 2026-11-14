package mysql

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"os"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/sriharip316/myhealthz/internal/config"
)

// tlsConfigName is the name registered with the MySQL driver for the
// TLS configuration built from flags.
const tlsConfigName = "myhealthz"

func buildTLSConfig(cfg *config.Config) (*tls.Config, error) {
	switch cfg.TLSMode {
	case "true", "skip-verify":
	default:
		return nil, nil
	}
	tlsConf := &tls.Config{ServerName: cfg.TLSServerName}
	if cfg.TLSMode == "skip-verify" {
		tlsConf.InsecureSkipVerify = true
	}
	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read -tls-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-tls-ca-file contains no valid certificates")
		}
		tlsConf.RootCAs = pool
	}
	if cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConf.Certificates = []tls.Certificate{cert}
	}
	return tlsConf, nil
}

// Open returns a connection pool for the endpoint. The database may not be
// reachable yet; callers should Ping separately and tolerate failure.
func Open(cfg *config.Config, ep Endpoint) (*sql.DB, error) {
	mc := gomysql.NewConfig()
	mc.User = cfg.User
	mc.Passwd = cfg.Password
	mc.Net = ep.Network
	mc.Addr = ep.Addr
	mc.Timeout = cfg.ConnectTimeout
	mc.ReadTimeout = cfg.QueryTimeout
	mc.WriteTimeout = cfg.QueryTimeout
	if ep.Network == "tcp" {
		tlsConf, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		if tlsConf != nil {
			if err := gomysql.RegisterTLSConfig(tlsConfigName, tlsConf); err != nil {
				return nil, fmt.Errorf("register TLS config: %w", err)
			}
			mc.TLSConfig = tlsConfigName
		}
	}
	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	// Health checks are cheap and frequent; a tiny pool is plenty.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(30 * time.Second)
	return db, nil
}
