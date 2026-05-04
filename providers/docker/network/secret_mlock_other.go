//go:build !linux

package network

// lockSecretPages is a no-op outside Linux. mlock has direct
// equivalents on macOS (mlock(2) via libc) and Windows
// (VirtualLock), but mkfst's mlock surface is Linux-only for v1.
// On other platforms secret bytes remain swap-eligible until
// Down zeroes them.
func lockSecretPages(_ []byte) error   { return nil }
func unlockSecretPages(_ []byte) error { return nil }
