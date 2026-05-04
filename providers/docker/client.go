// Package docker is mkfst's Docker engine wrapper.
//
// The provider exposes Pull/Build/Run/Logs/Stop/Remove operations as
// "required positional + functional options" calls, mirroring how mkfst's
// own tonic and fizz packages decorate per-request work. Every Docker option
// is a first-class typed function in this package — no opaque escape
// hatches:
//
//	events, err := client.Build(ctx, vfs,
//	    docker.Tag("my-app:v1"),
//	    docker.Arg("VERSION", "1.0"),
//	    docker.BuildPlatform("linux/amd64"),
//	    docker.Pull(),
//	)
//
// Build context defaults to in-memory via mkfst's engine/vfs package; a
// VFSSource is the documented norm. DirSource (host directory) and TarSource
// (passthrough tar stream) are also supported when needed.
//
// Long-running operations (Build, Pull, Logs) return event channels rather
// than blocking. Callers stream events lock-step and the channel closes when
// the operation completes; a terminal error appears as an EventError before
// the close.
//
// The wrapper is safe for concurrent use — every method takes its own
// per-request docker SDK context internally and the underlying SDK client
// is itself concurrent-safe.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/docker/docker/client"
)

// ErrUnreachable is returned by New (and Ping) when no Docker daemon is
// reachable at the configured Host. Callers can branch on this to fall back
// to a documented "Docker not available" path.
var ErrUnreachable = errors.New("docker: daemon unreachable")

// Opts configures a Client.
type Opts struct {
	// Host is the daemon address. Examples: "unix:///var/run/docker.sock",
	// "tcp://192.168.99.100:2376", "npipe:////./pipe/docker_engine".
	// If empty, the daemon address is taken from $DOCKER_HOST, falling back
	// to the platform default (unix socket on Linux/macOS, named pipe on
	// Windows).
	Host string

	// APIVersion pins the docker API version. Empty means "negotiate at
	// connection time" — the client and server agree on the highest version
	// they both support. Pinning is useful when reproducibility matters
	// across daemon upgrades.
	APIVersion string

	// HTTPClient overrides the default HTTP client used for communication
	// with the daemon. Useful for TLS configuration, custom timeouts, or
	// instrumented transports. When nil, the SDK builds an appropriate
	// client from the host scheme.
	HTTPClient *http.Client

	// Headers are added to every request. Useful for daemon-side identity
	// (User-Agent overrides, request IDs, etc.). The SDK adds its own
	// User-Agent if this map doesn't contain one.
	Headers map[string]string

	// Timeout bounds the connect-and-handshake portion of New. Operations
	// (Pull/Build/Run/...) take their own ctx and are not constrained by
	// this value. Default 10s; 0 disables.
	Timeout time.Duration

	// AllowVersionMismatch skips the API version negotiation. Off by default
	// because version mismatch usually surfaces as confusing errors deep in
	// other operations; failing fast at New is friendlier.
	AllowVersionMismatch bool
}

// Client is the high-level Docker engine wrapper. Construct one with New
// and reuse it across operations — it's safe for concurrent use.
type Client struct {
	cli  *client.Client
	api  dockerAPI // narrow interface; usually == cli, swapped in tests
	opts Opts
}

// New connects to the Docker daemon described by opts and returns a Client.
// Performs a Ping to fail fast on unreachable daemons; the returned error
// wraps ErrUnreachable in that case so callers can branch on it.
func New(opts Opts) (*Client, error) {
	dockerOpts := []client.Opt{}
	if opts.Host != "" {
		dockerOpts = append(dockerOpts, client.WithHost(opts.Host))
	}
	if opts.APIVersion != "" {
		dockerOpts = append(dockerOpts, client.WithVersion(opts.APIVersion))
	} else if !opts.AllowVersionMismatch {
		dockerOpts = append(dockerOpts, client.WithAPIVersionNegotiation())
	}
	if opts.HTTPClient != nil {
		dockerOpts = append(dockerOpts, client.WithHTTPClient(opts.HTTPClient))
	}
	if len(opts.Headers) > 0 {
		dockerOpts = append(dockerOpts, client.WithHTTPHeaders(opts.Headers))
	}
	if opts.Host == "" {
		dockerOpts = append(dockerOpts, client.FromEnv)
	}

	cli, err := client.NewClientWithOpts(dockerOpts...)
	if err != nil {
		return nil, fmt.Errorf("docker.New: %w", err)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if _, err := cli.Ping(ctx); err != nil {
			_ = cli.Close()
			return nil, fmt.Errorf("docker.New: ping %s: %w", cli.DaemonHost(), errors.Join(err, ErrUnreachable))
		}
	}

	return &Client{cli: cli, api: cli, opts: opts}, nil
}

// fromAPI is the test-only constructor: builds a Client around a
// mock dockerAPI. Methods that touch the concrete *client.Client
// (Ping/Host/Close/SDK) will panic — callers in tests should
// avoid those paths.
func fromAPI(api dockerAPI) *Client {
	return &Client{api: api}
}

// SDK returns the underlying docker client. Provided as an escape hatch for
// callers that need an API method this wrapper doesn't expose. Callers must
// not Close the returned client — Client.Close handles that.
func (c *Client) SDK() *client.Client { return c.cli }

// Host returns the daemon address the client is connected to.
func (c *Client) Host() string { return c.cli.DaemonHost() }

// Ping checks daemon reachability. Returns ErrUnreachable wrapping the
// underlying network error.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.cli.Ping(ctx); err != nil {
		return fmt.Errorf("docker.Ping: %w", errors.Join(err, ErrUnreachable))
	}
	return nil
}

// Close releases the underlying SDK client. After Close, the Client must
// not be used further. Idempotent.
func (c *Client) Close() error {
	return c.cli.Close()
}
