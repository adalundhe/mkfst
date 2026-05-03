package network

import (
	"crypto/rand"
	"encoding/hex"
)

// Labels applied to every docker resource the network module creates.
// They serve two purposes:
//
//   - Crash recovery: on engine restart, a process can list all
//     resources owned by a prior engine and choose to adopt
//     (resume management) or reap (clean teardown). See recovery.go.
//   - Observability: docker tooling and external observers can filter
//     by `mkfst.stack=<id>` to inspect one stack's resources.
const (
	// LabelManagedBy marks resources as belonging to mkfst. Always
	// set to "mkfst-network".
	LabelManagedBy = "mkfst.managed-by"
	// LabelEngineID is the engine identifier — uniquely identifies
	// the mkfst process instance that created the resource. Stable
	// across restarts only if the user supplies a deterministic ID
	// via EngineOpts.EngineID.
	LabelEngineID = "mkfst.engine"
	// LabelStackID is the stack the resource belongs to. Containers
	// and networks share the same value; orphan reaping uses this
	// label to bulk-target a stack.
	LabelStackID = "mkfst.stack"
	// LabelStackName is the human-readable stack name (the user-
	// supplied name passed to NewStack). Decoupled from StackID so
	// callers can re-create a stack with the same name without
	// adopting prior state if they don't want to.
	LabelStackName = "mkfst.stack-name"
	// LabelService is set on container resources only — identifies
	// which service within the stack this container is.
	LabelService = "mkfst.service"
	// LabelReplica is set on container resources only — the
	// 0-indexed replica number. For non-replicated services it's
	// always "0".
	LabelReplica = "mkfst.replica"
	// LabelRole distinguishes container kinds within a stack:
	// "service" (a normal user service), "init" (run-once init
	// container), "sidecar" (long-running sidecar started before
	// the main service).
	LabelRole = "mkfst.role"
	// LabelKind distinguishes resource kinds: "service", "network",
	// "secret-volume", etc. Useful for filter-by-kind queries.
	LabelKind = "mkfst.kind"
)

// Role values for LabelRole.
const (
	RoleService = "service"
	RoleInit    = "init"
	RoleSidecar = "sidecar"
)

// Kind values for LabelKind.
const (
	KindService = "service"
	KindNetwork = "network"
	KindSecret  = "secret-volume"
)

// stackLabels returns the base label set for a resource that belongs
// to a specific stack. Service-specific labels are layered on top by
// callers via withServiceLabels.
func stackLabels(engineID, stackID, stackName, kind string) map[string]string {
	return map[string]string{
		LabelManagedBy: "mkfst-network",
		LabelEngineID:  engineID,
		LabelStackID:   stackID,
		LabelStackName: stackName,
		LabelKind:      kind,
	}
}

// withServiceLabels returns a fresh map with the base stack labels
// plus service / replica / role identifiers for a container.
func withServiceLabels(base map[string]string, service string, replica int, role string) map[string]string {
	out := make(map[string]string, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	out[LabelService] = service
	out[LabelReplica] = formatReplica(replica)
	out[LabelRole] = role
	return out
}

func formatReplica(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + formatReplica(-n)
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	return string(buf[pos:])
}

// newID returns a 16-hex-char random ID suitable for stack and
// engine identifiers.
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
