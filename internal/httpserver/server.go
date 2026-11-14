// Package httpserver exposes myhealthz over HTTP.
package httpserver

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/sriharip316/myhealthz/internal/health"
	"github.com/sriharip316/myhealthz/internal/mysql"
)

// FactsProvider is implemented by *mysql.Collector; tests fake it.
type FactsProvider interface {
	Facts(ctx context.Context) (*mysql.Facts, error)
}

// Server serves the health endpoints.
type Server struct {
	provider    FactsProvider
	minReplicas int
	maxLag      int
	mu          sync.Mutex
	lastRole    string
}

// New builds a Server. minReplicas and maxLag mirror the -min-replicas and
// -max-lag flags.
func New(p FactsProvider, minReplicas, maxLagSeconds int) *Server {
	return &Server{provider: p, minReplicas: minReplicas, maxLag: maxLagSeconds, lastRole: "unknown"}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/primary", s.handleHealth("primary"))
	mux.HandleFunc("/health/replica", s.handleHealth("replica"))
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

// HealthResponse is the body of /health/primary and /health/replica.
type HealthResponse struct {
	OK       bool         `json:"ok"`
	Endpoint string       `json:"endpoint"`
	Role     string       `json:"role"`
	Reasons  []string     `json:"reasons"`
	Error    string       `json:"error,omitempty"`
	Facts    *mysql.Facts `json:"facts,omitempty"`
}

// StatusResponse is the body of /status.
type StatusResponse struct {
	OK       bool             `json:"ok"`
	Role     string           `json:"role"`
	Error    string           `json:"error,omitempty"`
	Decision *health.Decision `json:"decision,omitempty"`
	Facts    *mysql.Facts     `json:"facts,omitempty"`
	Config   StatusConfig     `json:"config"`
}

// StatusConfig echoes the settings that shaped the decision.
type StatusConfig struct {
	MinReplicas   int `json:"min_replicas"`
	MaxLagSeconds int `json:"max_lag_seconds"`
}

func (s *Server) handleHealth(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		facts, err := s.provider.Facts(r.Context())
		if err != nil {
			s.noteRole("error")
			writeJSON(w, http.StatusServiceUnavailable, HealthResponse{
				OK:       false,
				Endpoint: endpoint,
				Role:     "error",
				Reasons:  []string{"database check failed"},
				Error:    err.Error(),
			})
			return
		}
		d := health.Decide(facts, s.minReplicas, s.maxLag)
		ok, reasons := d.Primary, d.PrimaryReasons
		if endpoint == "replica" {
			ok, reasons = d.Replica, d.ReplicaReasons
		}
		role := "none"
		switch {
		case d.Primary:
			role = "primary"
		case d.Replica:
			role = "replica"
		}
		s.noteRole(role)
		code := http.StatusOK
		if !ok {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, HealthResponse{OK: ok, Endpoint: endpoint, Role: role, Reasons: reasons, Facts: facts})
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := StatusResponse{Config: StatusConfig{MinReplicas: s.minReplicas, MaxLagSeconds: s.maxLag}}
	facts, err := s.provider.Facts(r.Context())
	if err != nil {
		resp.Role = "error"
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	d := health.Decide(facts, s.minReplicas, s.maxLag)
	resp.OK = d.Primary || d.Replica
	switch {
	case d.Primary:
		resp.Role = "primary"
	case d.Replica:
		resp.Role = "replica"
	default:
		resp.Role = "none"
	}
	resp.Decision = &d
	resp.Facts = facts
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "myhealthz",
		"endpoints": []string{"/health/primary", "/health/replica", "/status", "/healthz"},
	})
}

// noteRole logs role transitions so switchover progress is visible in logs.
func (s *Server) noteRole(role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if role != s.lastRole {
		log.Printf("myhealthz: role changed %q -> %q", s.lastRole, role)
		s.lastRole = role
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
