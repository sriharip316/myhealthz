// Package health decides whether a server is acting as primary or replica
// from a Facts snapshot.
package health

import (
	"fmt"

	"github.com/sriharip316/myhealthz/internal/mysql"
)

// Decision is the role verdict for one server, with the reasons that went
// into it. Reasons are empty when the respective verdict is positive.
type Decision struct {
	Primary        bool     `json:"primary"`
	Replica        bool     `json:"replica"`
	PrimaryReasons []string `json:"primary_reasons"`
	ReplicaReasons []string `json:"replica_reasons"`
}

// Primary: read_only/super_read_only off, replicating from nobody, and at
// least minReplicas connected to it.
//
// Replica: read_only/super_read_only on and replication running from another
// server (both threads), within maxLagSeconds when enabled (maxLag < 0
// disables the lag check).
//
// Anything else — e.g. a stopped replica, or a promoted node whose replicas
// have not reconnected yet — is neither, which load balancers treat as
// "do not route traffic here".
func Decide(f *mysql.Facts, minReplicas, maxLagSeconds int) Decision {
	var d Decision
	if f == nil {
		// Nil snapshot (e.g. a provider returning (nil, nil)): treat as no
		// facts, which fails closed as "neither" below.
		f = &mysql.Facts{}
	}
	readOnly := f.ReadOnly || f.SuperReadOnly

	if readOnly {
		d.PrimaryReasons = append(d.PrimaryReasons, "read_only/super_read_only is ON")
	}
	if f.ReplicaConfigured {
		d.PrimaryReasons = append(d.PrimaryReasons, fmt.Sprintf("replication configured (source %s)", f.SourceHost))
	}
	if f.ConnectedReplicas < minReplicas {
		d.PrimaryReasons = append(d.PrimaryReasons, fmt.Sprintf("connected replicas %d < required %d", f.ConnectedReplicas, minReplicas))
	}
	d.Primary = !readOnly && !f.ReplicaConfigured && f.ConnectedReplicas >= minReplicas

	if !readOnly {
		d.ReplicaReasons = append(d.ReplicaReasons, "read_only/super_read_only is OFF")
	}
	if !f.ReplicaConfigured {
		d.ReplicaReasons = append(d.ReplicaReasons, "not replicating from any server")
	} else {
		if f.IORunning != "Yes" {
			d.ReplicaReasons = append(d.ReplicaReasons, fmt.Sprintf("replication IO thread is %q", f.IORunning))
		}
		if f.SQLRunning != "Yes" {
			d.ReplicaReasons = append(d.ReplicaReasons, fmt.Sprintf("replication SQL thread is %q", f.SQLRunning))
		}
		if maxLagSeconds >= 0 {
			switch {
			case f.SecondsBehindSource == nil:
				d.ReplicaReasons = append(d.ReplicaReasons, "replication lag is unknown")
			case *f.SecondsBehindSource > int64(maxLagSeconds):
				d.ReplicaReasons = append(d.ReplicaReasons, fmt.Sprintf("replication lag %ds exceeds max %ds", *f.SecondsBehindSource, maxLagSeconds))
			}
		}
	}
	lagOK := maxLagSeconds < 0 ||
		(f.SecondsBehindSource != nil && *f.SecondsBehindSource <= int64(maxLagSeconds))
	d.Replica = readOnly && f.ReplicaConfigured &&
		f.IORunning == "Yes" && f.SQLRunning == "Yes" && lagOK

	return d
}
