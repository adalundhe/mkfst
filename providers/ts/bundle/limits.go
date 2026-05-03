package bundle

// === resource caps ===
//
// Hard limits enforced at every submit. These are deliberately
// strict — workflow authors are users, not operators, and a
// compromised author should not be able to DOS the server, fill
// disks, exhaust memory, or otherwise harm the host.
//
// All limits are configurable via Opts but apply by default.

const (
	// DefaultMaxSourceBytes is the cap on raw TS source we
	// accept at submit time. 256 KiB is enough for almost any
	// real workflow; larger values are blocked early so we never
	// hand them to esbuild.
	DefaultMaxSourceBytes = 256 * 1024

	// DefaultMaxBundleBytes caps the post-bundling JS output.
	// 1 MiB allows generous helper code from blessed modules
	// while still being far below memory-pressure territory.
	DefaultMaxBundleBytes = 1024 * 1024

	// DefaultMaxIdentLen caps any single identifier name (function,
	// variable, class) anywhere in the source. Defends against
	// identifier-name bombs (megabyte-long names that bloat error
	// messages, source maps, etc.).
	DefaultMaxIdentLen = 256

	// DefaultMaxStringLiteralBytes caps the byte length of any
	// single string literal in the source. Defends against giant
	// inline payloads.
	DefaultMaxStringLiteralBytes = 64 * 1024

	// DefaultMaxImports caps total resolved imports per workflow.
	DefaultMaxImports = 256
)

// === DAG sanity ===

const (
	// DefaultMaxTasksPerWorkflow caps the number of defineTask
	// calls.
	DefaultMaxTasksPerWorkflow = 1000

	// DefaultMaxNodesPerWorkflow caps the number of DAG nodes
	// (b.add calls).
	DefaultMaxNodesPerWorkflow = 5000

	// DefaultMaxParentsPerNode caps a node's fan-in.
	DefaultMaxParentsPerNode = 50

	// DefaultMaxDAGDepth caps the longest path through the DAG.
	DefaultMaxDAGDepth = 100
)
