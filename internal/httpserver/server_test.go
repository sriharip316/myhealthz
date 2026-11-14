package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sriharip316/myhealthz/internal/mysql"
)

type fakeProvider struct {
	facts *mysql.Facts
	err   error
}

func (p *fakeProvider) Facts(ctx context.Context) (*mysql.Facts, error) {
	return p.facts, p.err
}

func newTestServer(p FactsProvider) *httptest.Server {
	return httptest.NewServer(New(p, 1, -1).Handler())
}

func TestHealthPrimaryEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		facts    *mysql.Facts
		wantCode int
		wantOK   bool
		wantRole string
	}{
		{name: "primary healthy", facts: &mysql.Facts{ConnectedReplicas: 2}, wantCode: 200, wantOK: true, wantRole: "primary"},
		{name: "replica is not primary", facts: &mysql.Facts{ReadOnly: true, ReplicaConfigured: true, IORunning: "Yes", SQLRunning: "Yes"}, wantCode: 503, wantOK: false, wantRole: "replica"},
		{name: "lone node is not primary", facts: &mysql.Facts{}, wantCode: 503, wantOK: false, wantRole: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(&fakeProvider{facts: tt.facts})
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/health/primary")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantCode)
			}
			var body HealthResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.OK != tt.wantOK || body.Role != tt.wantRole || body.Endpoint != "primary" {
				t.Fatalf("body = %+v", body)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type = %q", ct)
			}
		})
	}
}

func TestHealthReplicaEndpoint(t *testing.T) {
	ts := newTestServer(&fakeProvider{facts: &mysql.Facts{
		ReadOnly: true, ReplicaConfigured: true, SourceHost: "db1", IORunning: "Yes", SQLRunning: "Yes",
	}})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health/replica")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Role != "replica" || len(body.Reasons) != 0 {
		t.Fatalf("body = %+v", body)
	}
	if body.Facts == nil || body.Facts.SourceHost != "db1" {
		t.Fatalf("facts missing in body: %+v", body.Facts)
	}
}

func TestHealthEndpointsOnDBError(t *testing.T) {
	ts := newTestServer(&fakeProvider{err: fmt.Errorf("dial tcp: connection refused")})
	defer ts.Close()

	for _, path := range []string{"/health/primary", "/health/replica"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", path, resp.StatusCode)
		}
		var body HealthResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.OK || body.Role != "error" || body.Error == "" {
			t.Fatalf("%s: body = %+v", path, body)
		}
	}
}

func TestStatusEndpointAlways200(t *testing.T) {
	ts := newTestServer(&fakeProvider{err: fmt.Errorf("boom")})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Role != "error" || body.Error == "" {
		t.Fatalf("body = %+v", body)
	}
}

func TestAuxEndpoints(t *testing.T) {
	ts := newTestServer(&fakeProvider{facts: &mysql.Facts{ConnectedReplicas: 1}})
	defer ts.Close()

	for path, wantCode := range map[string]int{"/healthz": 200, "/": 200, "/nope": 404} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != wantCode {
			t.Fatalf("%s: status = %d, want %d", path, resp.StatusCode, wantCode)
		}
	}
}
