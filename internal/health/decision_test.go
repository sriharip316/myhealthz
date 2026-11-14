package health

import (
	"strings"
	"testing"

	"github.com/sriharip316/myhealthz/internal/mysql"
)

func primaryFacts() *mysql.Facts {
	return &mysql.Facts{
		ReadOnly:          false,
		SuperReadOnly:     false,
		ReplicaConfigured: false,
		ConnectedReplicas: 2,
	}
}

func replicaFacts() *mysql.Facts {
	return &mysql.Facts{
		ReadOnly:          true,
		ReplicaConfigured: true,
		SourceHost:        "db1",
		IORunning:         "Yes",
		SQLRunning:        "Yes",
	}
}

func TestDecide(t *testing.T) {
	lag10 := int64(10)
	tests := []struct {
		name            string
		facts           *mysql.Facts
		minReplicas     int
		maxLag          int
		wantPrimary     bool
		wantReplica     bool
		wantReasonIn    string // expected substring in the failing side's reasons
		reasonOnPrimary bool
	}{
		{
			name: "healthy primary", facts: primaryFacts(), minReplicas: 1,
			wantPrimary: true,
		},
		{
			name:  "primary without enough connected replicas",
			facts: &mysql.Facts{ConnectedReplicas: 0}, minReplicas: 1,
			wantPrimary: false, wantReasonIn: "connected replicas 0 < required 1", reasonOnPrimary: true,
		},
		{
			name:  "primary below configurable replica count",
			facts: &mysql.Facts{ConnectedReplicas: 1}, minReplicas: 2,
			wantPrimary: false, wantReasonIn: "connected replicas 1 < required 2", reasonOnPrimary: true,
		},
		{
			name:  "read-only node is not primary",
			facts: &mysql.Facts{ReadOnly: true, ConnectedReplicas: 3}, minReplicas: 1,
			wantPrimary: false, wantReasonIn: "read_only/super_read_only is ON", reasonOnPrimary: true,
		},
		{
			name:  "super_read_only alone blocks primary (MySQL)",
			facts: &mysql.Facts{SuperReadOnly: true, SuperReadOnlySupported: true, ConnectedReplicas: 3}, minReplicas: 1,
			wantPrimary: false, wantReasonIn: "read_only/super_read_only is ON", reasonOnPrimary: true,
		},
		{
			name:  "node replicating from someone is not primary",
			facts: &mysql.Facts{ReplicaConfigured: true, SourceHost: "db9", ConnectedReplicas: 3}, minReplicas: 1,
			wantPrimary: false, wantReasonIn: "replication configured", reasonOnPrimary: true,
		},
		{
			name: "healthy replica", facts: replicaFacts(),
			wantReplica: true,
		},
		{
			name:         "writable replica-configured node is neither (mid-switchover)",
			facts:        &mysql.Facts{ReplicaConfigured: true, SourceHost: "db1", IORunning: "No", SQLRunning: "No", ConnectedReplicas: 2},
			minReplicas:  1,
			wantReasonIn: "read_only/super_read_only is OFF", reasonOnPrimary: false,
		},
		{
			name:         "replica with stopped IO thread is not replica",
			facts:        &mysql.Facts{ReadOnly: true, ReplicaConfigured: true, IORunning: "No", SQLRunning: "Yes"},
			wantReasonIn: `replication IO thread is "No"`, reasonOnPrimary: false,
		},
		{
			name:         "writable node without connected replicas is neither",
			facts:        &mysql.Facts{},
			wantReasonIn: "read_only/super_read_only is OFF", reasonOnPrimary: false,
		},
		{
			name:        "replica within max lag",
			facts:       &mysql.Facts{ReadOnly: true, ReplicaConfigured: true, IORunning: "Yes", SQLRunning: "Yes", SecondsBehindSource: &lag10},
			maxLag:      30,
			wantReplica: true,
		},
		{
			name:         "replica exceeding max lag",
			facts:        &mysql.Facts{ReadOnly: true, ReplicaConfigured: true, IORunning: "Yes", SQLRunning: "Yes", SecondsBehindSource: &lag10},
			maxLag:       5,
			wantReasonIn: "replication lag 10s exceeds max 5s", reasonOnPrimary: false,
		},
		{
			name:         "replica with unknown lag fails when max lag enabled",
			facts:        &mysql.Facts{ReadOnly: true, ReplicaConfigured: true, IORunning: "Yes", SQLRunning: "Yes"},
			maxLag:       5,
			wantReasonIn: "replication lag is unknown", reasonOnPrimary: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxLag := tt.maxLag
			if maxLag == 0 { // unset table field; 0 ("require zero lag") is not exercised
				maxLag = -1
			}
			minReplicas := tt.minReplicas
			if minReplicas == 0 { // unset table field; the runtime default is 1
				minReplicas = 1
			}
			d := Decide(tt.facts, minReplicas, maxLag)
			if d.Primary != tt.wantPrimary {
				t.Errorf("Primary = %v, want %v (reasons: %v)", d.Primary, tt.wantPrimary, d.PrimaryReasons)
			}
			if d.Replica != tt.wantReplica {
				t.Errorf("Replica = %v, want %v (reasons: %v)", d.Replica, tt.wantReplica, d.ReplicaReasons)
			}
			if tt.wantReasonIn != "" {
				reasons := d.PrimaryReasons
				if !tt.reasonOnPrimary {
					reasons = d.ReplicaReasons
				}
				found := false
				for _, r := range reasons {
					if strings.Contains(r, tt.wantReasonIn) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no reason contains %q; got primary=%v replica=%v", tt.wantReasonIn, d.PrimaryReasons, d.ReplicaReasons)
				}
			}
		})
	}
}

func TestDecideNilFactsFailsClosed(t *testing.T) {
	// A nil snapshot must not panic and must classify as neither, so a
	// misbehaving provider quarantines the node instead of crashing it.
	d := Decide(nil, 1, -1)
	if d.Primary || d.Replica {
		t.Fatalf("Primary = %v, Replica = %v, want both false", d.Primary, d.Replica)
	}
}
