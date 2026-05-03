// Package network is mkfst's container-stack orchestration submodule of
// providers/docker. It provides a Compose-like model — multiple
// containers grouped as services on a private bridge network — with
// stronger guarantees around atomicity, isolation, observability, and
// scale than docker-compose offers.
//
// Why this exists in mkfst rather than as a wrapper around compose:
//
//   - mkfst owns its docker provider and can layer first-class
//     ergonomics over the SDK without shelling out to the compose CLI.
//   - Compose's lifecycle is "best-effort": services can leak on
//     partial failure, and there's no atomic adopt-or-rollback. mkfst
//     uses a documented state machine and two-phase Up to prevent
//     leaks.
//   - mkfst processes need to *talk to* their stacks and route traffic
//     into them with enforced rules. An in-process gateway gives us
//     L4/L7 control without sidecars.
//
// Concept summary:
//
//   - Stack: a named bundle of Services on a dedicated bridge network.
//     Two stacks cannot reach each other (separate networks, separate
//     DNS) but every service may reach the external internet (default
//     bridge NAT).
//   - Service: a containerized program with image + env + cmd +
//     volumes + dependencies + probe + replicas. Probes are TCP/HTTP/
//     UDP/gRPC/Exec; mode is Readiness or Liveness, picked by user.
//   - Ingress: a host-side entrypoint owned by the stack; a pure-Go
//     gateway listens, applies allow/deny rules, monitors traffic,
//     and forwards to the right backend (replica-aware).
//   - Egress: per-service controls on what external endpoints a
//     container may reach (CIDR or domain). Domain enforcement uses
//     a per-stack DNS resolver.
//
// Scale targets: thousands of concurrent stacks per host. Hot paths
// are lock-free (atomic.Pointer for rules and probe state). Probe
// scheduling uses one process-wide min-heap with a bounded worker
// pool. The ingress acceptor uses sync.Pool buffers and the
// kernel splice() fast-path on Linux for zero-copy forwarding.
//
// Cross-platform: Linux, macOS, Windows. The API surface is identical;
// OS-specific fast paths (Unix sockets for backend reachability on
// Linux, splice for forwarding) auto-detect.
//
// Resource lifecycle: every docker resource carries
// `mkfst.engine=<id>` + `mkfst.stack=<id>` labels. On engine restart,
// resources from prior processes can be discovered and either adopted
// (continue managing them) or reaped (clean shutdown of the prior
// world). Stack.Up and Stack.Down are idempotent.
package network
