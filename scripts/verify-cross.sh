#!/usr/bin/env bash
# verify-cross.sh — sanity-check the providers/vfs source tree compiles
# (or at least parses) for every supported GOOS without requiring a CGO
# cross-toolchain on the host.
#
# What this catches:
#   - Syntax errors in any platform-conditional file
#   - Missing imports / API drift against cgofuse on darwin/windows
#   - Bad build tags
#
# What this does NOT catch:
#   - C linker errors (need real cross-CC: clang/mingw/zig)
#   - Runtime driver issues (macFUSE/WinFsp not installed)
#
# Run on Linux without any darwin/windows toolchain installed.

set -euo pipefail
cd "$(dirname "$0")/.."

echo "=== gofmt"
unformatted=$(gofmt -l providers/vfs/)
if [[ -n "$unformatted" ]]; then
    echo "Files not gofmt'd:" >&2
    echo "$unformatted" >&2
    exit 1
fi

echo "=== Linux build (default)"
go build ./providers/vfs/

echo "=== Linux test (unit)"
go test -count=1 ./providers/vfs/ >/dev/null

echo "=== Linux test (fuse integration, if /dev/fuse available)"
if [[ -e /dev/fuse ]]; then
    go test -tags fuse_integration -count=1 ./providers/vfs/ >/dev/null
else
    echo "/dev/fuse missing — skipping fuse_integration"
fi

echo "=== Cross-OS source check (no CGO)"
for goos in darwin windows freebsd; do
    echo "  -> GOOS=$goos"
    # vet picks up syntax/import errors. We filter cgofuse's own source
    # noise (its .go files reference cgo-defined consts that are invisible
    # without a C compiler — expected, not our problem).
    out=$(GOOS=$goos CGO_ENABLED=0 go vet ./providers/vfs/ 2>&1 || true)
    ours=$(echo "$out" | grep -v "/pkg/mod/" | grep -E "providers/vfs/|^[a-z]+\.go" || true)
    if [[ -n "$ours" ]]; then
        echo "FAIL: providers/vfs source has errors under GOOS=$goos:"
        echo "$ours"
        exit 1
    fi
done

echo
echo "=== cgofuse API surface check"
# Grep cgofuse to verify the methods/types/constants the file references
# all still exist in the pinned version.
mod=$(go env GOMODCACHE)/github.com/winfsp/cgofuse@*
needed=(
    "func (\*FileSystemBase) Init"
    "func (\*FileSystemBase) Statfs"
    "func (\*FileSystemBase) Getattr"
    "func (\*FileSystemBase) Opendir"
    "func (\*FileSystemBase) Readdir"
    "func (\*FileSystemBase) Open"
    "func (\*FileSystemBase) Read"
    "func (\*FileSystemBase) Write"
    "func (\*FileSystemBase) Mknod"
    "func (\*FileSystemBase) Create"
    "func (\*FileSystemBase) Mkdir"
    "func (\*FileSystemBase) Unlink"
    "func (\*FileSystemBase) Rmdir"
    "func (\*FileSystemBase) Rename"
    "func (\*FileSystemBase) Symlink"
    "func (\*FileSystemBase) Readlink"
    "func (\*FileSystemBase) Chmod"
    "func (\*FileSystemBase) Chown"
    "func (\*FileSystemBase) Truncate"
    "func (\*FileSystemBase) Utimens"
    "func NewFileSystemHost"
    "Mount(mountpoint string, opts \[\]string)"
    "Unmount() bool"
    "type Stat_t"
    "type Statfs_t"
    "type Timespec"
)
missing=0
for sig in "${needed[@]}"; do
    if ! grep -rq -- "$sig" $mod/fuse/ 2>/dev/null; then
        echo "  MISSING: $sig"
        missing=$((missing+1))
    fi
done
if (( missing > 0 )); then
    echo "FAIL: $missing missing cgofuse symbols — pin may have drifted"
    exit 1
fi
echo "OK: all $((${#needed[@]})) cgofuse symbols present"

echo
echo "All cross-OS source checks passed."
echo "To verify the C linker step on macOS/Windows, build there directly."
