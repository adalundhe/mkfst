# VFS (Virtual Filesystem)

`providers/vfs` is mkfst's in-memory filesystem with optional FUSE
mounting and a host-overlay cache. It exists to:

- Serve as the in-memory build context for `providers/docker.Build`.
- Mount as a real filesystem (FUSE) so containers and host tools can
  read/write a coherent view.
- Provide a copy-on-write layer over a (potentially huge) base
  directory: only the changes are kept in memory; reads of unchanged
  files fall through to the host overlay (with caching).

## Tree

```go
import "mkfst/providers/vfs"

tree := vfs.New()
defer tree.Close()

_ = tree.WriteFile("/etc/myapp.conf", []byte("key=value"), 0o644)
_ = tree.MkdirAll("/var/log", 0o755)
b, _ := tree.ReadFile("/etc/myapp.conf")
```

The Tree API is small and Go-idiomatic: `WriteFile`, `ReadFile`,
`Mkdir`, `MkdirAll`, `Remove`, `RemoveAll`, `Stat`, `ReadDir`,
`Rename`, `Truncate`, `Symlink`.

## Host overlay (CoW)

Mount a host directory as the read-through base; mutations stay in
the in-memory layer:

```go
tree := vfs.New(vfs.WithHostOverlay("/var/lib/myapp/seed"))
// Reads of unchanged paths come from /var/lib/myapp/seed.
// Writes land in the in-memory layer, never touching the host.
b, _ := tree.ReadFile("/large-fixture.bin") // streams from disk via cache
_ = tree.WriteFile("/large-fixture.bin", []byte("modified"), 0o644)
// Subsequent reads see the modified bytes; the host file is unchanged.
```

The overlay cache (`providers/vfs/host_cache.go`) is content-
addressable: repeated reads of an unchanged host file are served from
memory. Stat-based invalidation: when the underlying file's
(size, mtime) changes, the cache evicts and re-reads.

This is the path that supports **20–100 GB seed datasets** without
loading them into memory — only the bytes you actually read are paged
in, only the changes you write are buffered.

## FUSE mount

```go
mp, err := vfs.NewMountpoint("/run/user/1000/myapp-mnt") // tmpfs path
defer mp.Close()
mount, err := tree.Mount(ctx, mp.Path)
defer mount.Unmount()
```

The mount is a real filesystem entry: any process — including
containers via `docker.Bind(mp.Path, "/data")` — sees the VFS through
normal POSIX I/O.

Backends:
- Linux: pure-Go [go-fuse](https://github.com/hanwen/go-fuse). No cgo
  required.
- macOS: cgofuse + macFUSE. Requires CGO and macFUSE installed.
- Windows: cgofuse + WinFsp.

The framework auto-selects per build platform; no user code change.

## Live mount (bidirectional sync)

`providers/docker/livemount` watches the VFS for changes and pushes
them to a container, and watches the container for changes and pulls
them back. Useful for development workflows where a host tool edits
a file and the container should see the update without rebuilding.

```go
import "mkfst/providers/docker/livemount"

session, err := livemount.Start(ctx, livemount.Opts{
    Tree:        tree,
    Client:      cli,
    ContainerID: id,
    Targets:     []string{"/etc/myapp", "/var/data"},
})
defer session.Close()
```

Direction:
- **VFS → container** is event-driven (FUSE notifications).
- **Container → VFS** is adaptive polling (1 Hz when idle, 10 Hz under
  detected change).

## Subscribe to changes

```go
ev, cancel := tree.Subscribe(ctx)
defer cancel()
for change := range ev {
    log.Printf("%s %s", change.Op, change.Path)
}
```

The subscriber loop is internally tracked; cancelling the ctx (or
calling cancel()) joins the goroutine cleanly.

## Files API

`providers/files` is a higher-level wrapper for common file-shape
operations against a VFS:

```go
import "mkfst/providers/files"

f := files.New(tree)
_ = f.WriteJSON("/cfg.json", map[string]any{"x": 1})
var v map[string]any
_ = f.ReadJSON("/cfg.json", &v)
_ = f.WriteText("/notes.md", "...")
```

## Composing with the HTTP server

A "user uploads a file, gets read back" flow on top of an in-memory
VFS:

```go
svc.Route("POST", "/files", 200, nil,
    func(c *gin.Context, _ *sql.DB) (struct{ Path string }, error) {
        body, _ := io.ReadAll(c.Request.Body)
        path := "/uploads/" + uuid.NewString()
        if err := tree.WriteFile(path, body, 0o644); err != nil {
            return struct{ Path string }{}, err
        }
        return struct{ Path string }{Path: path}, nil
    },
)

svc.Route("GET", "/files/*path", 200, nil,
    func(c *gin.Context, _ *sql.DB, in *struct {
        Path string `path:"path"`
    }) ([]byte, error) {
        return tree.ReadFile(in.Path)
    },
)
```

## See also

- [docker.md](docker.md) — VFS as build context
- [stacks.md](stacks.md) — VFS-backed mounts inside stack services
