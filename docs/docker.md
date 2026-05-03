# Docker

`providers/docker` is mkfst's Docker engine wrapper. It exposes
Pull / Build / Run / Logs / Stop / Remove / Inspect as
"required positional + functional options" calls — the same shape as
mkfst's own tonic / fizz handlers.

```go
events, err := client.Build(ctx, vfs,
    docker.Tag("my-app:v1"),
    docker.Arg("VERSION", "1.0"),
    docker.BuildPlatform("linux/amd64"),
    docker.Pull(),
)
```

Build context defaults to in-memory via [`providers/vfs`](vfs.md);
`DirSource` (host directory) and `TarSource` (passthrough tar stream)
are also supported.

## Construction

```go
import dockerprov "mkfst/providers/docker"

cli, err := dockerprov.New(dockerprov.Opts{
    Host:    "", // empty = $DOCKER_HOST or platform default
    Timeout: 5 * time.Second,
})
defer cli.Close()
```

`New` performs a `Ping` to fail fast on unreachable daemons; the
returned error wraps `dockerprov.ErrUnreachable`.

`cli.SDK()` exposes the underlying `docker/client.Client` for
SDK-only operations not wrapped here.

## Pull

```go
events, err := cli.Pull(ctx, "alpine:3.19")
for ev := range events {
    switch ev.Kind {
    case docker.EventStatus:
        log.Printf("[%s] %s", ev.Status, ev.Message)
    case docker.EventError:
        return ev.Err
    }
}
```

Pulls produce a stream of `Event`s. The channel closes when the pull
completes; a terminal error appears as an `EventError`.

## Build

In-memory VFS context (the documented norm):

```go
import "mkfst/providers/vfs"

tree := vfs.New()
_ = tree.WriteFile("Dockerfile", []byte("FROM alpine\nCMD echo hi"), 0644)
_ = tree.WriteFile("app.txt", []byte("payload"), 0644)

events, err := cli.Build(ctx, tree,
    docker.Tag("hi:dev"),
    docker.NoCache(),
)
```

Host directory:

```go
events, err := cli.Build(ctx, docker.DirSource("./build-context"), ...)
```

## Run

```go
result, err := cli.Run(ctx, "alpine:3.19",
    docker.Cmd("echo", "hello"),
    docker.Env("APP_ENV", "prod"),
    docker.Port(docker.PortMap{ContainerPort: 8080}), // ephemeral host port
    docker.AutoRemove(),
    docker.WaitForExit(), // sync mode; blocks until exit
)
log.Printf("exit %d", result.ExitCode)
```

By default `Run` returns as soon as the container is started. Pass
`WaitForExit()` to block until exit and surface the exit code.

## Logs

Streamed via channel:

```go
logs, err := cli.Logs(ctx, containerID, docker.Follow())
for line := range logs {
    if line.Err != nil { break }
    fmt.Printf("[%s] %s\n", line.Stream, line.Message)
}
```

## Lifecycle

```go
_ = cli.Stop(ctx, containerID, &timeoutSeconds)
_ = cli.Kill(ctx, containerID, "SIGKILL")
_ = cli.Restart(ctx, containerID, &timeoutSeconds)
_ = cli.Remove(ctx, containerID, docker.RemoveOpts{Force: true, RemoveVolumes: true})
exitCode, _ := cli.Wait(ctx, containerID, docker.WaitConditionNotRunning)
info, _ := cli.Inspect(ctx, containerID)
list, _ := cli.List(ctx, container.ListOptions{All: true})
```

## Networks (low-level)

`providers/docker/network` exposes typed network primitives + Stacks
(see [stacks.md](stacks.md)). The low-level surface:

```go
import "mkfst/providers/docker/network"

n, err := network.Create(ctx, cli.SDK(),
    "engine-id", "stack-id", "stack-name",
    "my-net",
    network.Driver("bridge"),
    network.Subnet("172.31.0.0/16"),
)
defer n.Remove(ctx)

_ = n.Connect(ctx, containerID, "alias-1", "alias-2")
```

## Live mount + sync

`providers/docker/livemount` and `providers/docker/sync` provide
bidirectional VFS↔container synchronization. See
[vfs.md](vfs.md#live-mount) for the full pattern.

## Cross-platform

- Linux: rootful and rootless docker. The provider auto-handles
  rootless DNS quirks.
- macOS / Windows: Docker Desktop. Same Go API, same operations;
  some ops (host bind mounts of FUSE paths) gated by daemon
  capabilities and skipped with clear errors.

## See also

- [stacks.md](stacks.md) — Compose-like multi-container apps
- [vfs.md](vfs.md) — In-memory FS used as build context + live-mount
- [examples/12-stacks](../examples/12-stacks) — runnable docker integration
