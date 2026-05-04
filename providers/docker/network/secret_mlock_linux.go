//go:build linux

package network

import "syscall"

// lockSecretPages calls mlock(2) on the secret bytes so they are
// not paged out to swap. Best-effort: when CAP_IPC_LOCK is not
// granted (typical for unprivileged user processes), mlock returns
// EPERM and we silently continue — the secret still lives only in
// RSS and gets zeroed on Down, which is the second line of defense.
func lockSecretPages(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Mlock(b)
}

// unlockSecretPages reverses lockSecretPages. Same best-effort
// posture: errors are non-actionable.
func unlockSecretPages(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Munlock(b)
}
