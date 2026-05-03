package files

import "os"

// execOSEnviron returns the current process's environment. Pulled into a
// platform-conditional file so future stubs (e.g. for sandboxed exec
// environments where os.Environ is restricted) can override.
func execOSEnviron() []string {
	return os.Environ()
}
