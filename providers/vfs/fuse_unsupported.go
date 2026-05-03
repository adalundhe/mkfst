//go:build !linux && !darwin && !windows

package vfs

// newMountDriver is the fallback for platforms with no FUSE backend wired
// in. We refuse explicitly so callers get a clear error rather than a
// confusing silent no-op or panic.
func newMountDriver(_ *Tree, _ MountOpts) (mountDriver, chan struct{}, error) {
	return nil, nil, ErrFUSEUnavailable
}
