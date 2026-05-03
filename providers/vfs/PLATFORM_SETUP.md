# providers/vfs — Platform setup

The VFS package compiles and runs on Linux, macOS, and Windows. The FUSE
backend uses different drivers on each platform:

| OS | Backend | Build deps | Runtime deps |
|---|---|---|---|
| Linux | `hanwen/go-fuse` (pure Go, no CGO) | none | `/dev/fuse` (kernel module — present on every standard distro) |
| macOS | `winfsp/cgofuse` over macFUSE | Xcode Command Line Tools (`clang`) | [macFUSE](https://osxfuse.github.io/) installed and kext-approved |
| Windows | `winfsp/cgofuse` over WinFsp | mingw-w64 (`gcc`) or MSVC | [WinFsp](https://winfsp.dev/) installed |

The Linux backend is pure-Go; binaries built on Linux for Linux are static
and need no runtime libraries. The macOS and Windows backends require CGO
and the corresponding userspace driver.

## macOS developer setup

```sh
# Build deps (one-time):
xcode-select --install

# Runtime deps:
brew install --cask macfuse
# Then approve the kext in System Settings → Privacy & Security
# (only needed once per machine).

# Build & test:
go build ./...
go test -tags fuse_integration ./providers/vfs/
```

## Windows developer setup

```powershell
# Build deps: install mingw-w64 (msys2 or winlibs), then:
go env -w CGO_ENABLED=1

# Runtime deps: install WinFsp from https://winfsp.dev/

# Build & test (PowerShell):
go build ./...
go test -tags fuse_integration ./providers/vfs/
```

## Cross-compilation

CGO cross-compilation requires either:
- **zig** as a CGO compiler driver: `CC="zig cc -target aarch64-macos-none" CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build`
- **osxcross** for Linux→macOS: `CC=o64-clang CGO_ENABLED=1 GOOS=darwin go build`
- **mingw-w64** for Linux→Windows: `CC=x86_64-w64-mingw32-gcc CGO_ENABLED=1 GOOS=windows go build`

If you don't have a cross-toolchain, build natively on each target OS.

## Verifying without a target machine

The CI sweep `scripts/verify-cross.sh` runs `gofmt`, `go vet` (without C
compilation), and a syntax-only parse over the cgofuse file under each
GOOS. It catches API drift against cgofuse but cannot validate the C
linker step — that requires a target machine.

## Why the choice of go-fuse on Linux but cgofuse on macOS/Windows

Linux's FUSE protocol is exposed as a documented kernel ABI over
`/dev/fuse`. `hanwen/go-fuse` speaks that protocol directly in Go, so
Linux binaries are CGO-free.

macOS and Windows don't expose an analogous protocol — macFUSE and WinFsp
are kernel drivers with their own C ABIs. There's no pure-Go alternative;
`winfsp/cgofuse` is the userspace shim that abstracts both.
