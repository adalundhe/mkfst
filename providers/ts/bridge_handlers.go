package ts

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"mkfst/providers/docker/network"
	"mkfst/providers/ts/runtime"
)

// === bridge handlers ===
//
// These are the Go-side implementations of host calls invoked by
// JS via `__mkfst_dispatch`. They translate the JS-side typed
// args (decoded from JSON) into stack primitive calls, run them,
// and return the typed result.
//
// All handlers are registered against a Bridge by RegisterStackHandlers.

// StackResolver maps a stack name (referenced from JS) to a live
// network.Stack. The resolver lookup happens per-call, so adding /
// removing stacks at runtime is reflected without re-registering
// handlers.
type StackResolver func(name string) (*network.Stack, bool)

// RegisterStackHandlers wires up stack.runOneShot, stack.exec,
// stack.address, stack.waitHealthy and log onto the bridge.
//
// resolver is called per-invocation to find the target stack —
// typically a closure over an in-memory map populated from
// mkfst.yaml stack-apply.
func RegisterStackHandlers(b *runtime.Bridge, resolver StackResolver) error {
	if b == nil {
		return errors.New("RegisterStackHandlers: nil bridge")
	}
	if resolver == nil {
		return errors.New("RegisterStackHandlers: nil resolver")
	}

	register := func(op string, h runtime.BridgeHandler) error {
		if err := b.Register(op, h); err != nil {
			return fmt.Errorf("register %s: %w", op, err)
		}
		return nil
	}

	if err := register("stack.runOneShot",
		runtime.JSON(func(bc runtime.BridgeCtx, args runOneShotArgs) (runOneShotResult, error) {
			s, ok := resolver(args.Stack)
			if !ok {
				return runOneShotResult{}, fmt.Errorf("stack %q not found", args.Stack)
			}
			opts := network.OneShotOpts{
				Image:      args.Image,
				Cmd:        args.Cmd,
				Entrypoint: args.Entrypoint,
				Env:        args.Env,
				WorkDir:    args.WorkDir,
				User:       args.User,
				Aliases:    args.Aliases,
			}
			if args.TimeoutSec > 0 {
				opts.Timeout = time.Duration(args.TimeoutSec) * time.Second
			}
			if args.Stdin != "" {
				opts.Stdin = []byte(args.Stdin)
			}
			res, err := s.RunOneShot(bc.Ctx, opts)
			if err != nil {
				return runOneShotResult{}, err
			}
			return runOneShotResult{
				ContainerID: res.ContainerID,
				ExitCode:    res.ExitCode,
				Stdout:      string(res.Stdout),
				Stderr:      string(res.Stderr),
				DurationMs:  res.Duration.Milliseconds(),
			}, nil
		})); err != nil {
		return err
	}

	if err := register("stack.exec",
		runtime.JSON(func(bc runtime.BridgeCtx, args execArgs) (execResult, error) {
			s, ok := resolver(args.Stack)
			if !ok {
				return execResult{}, fmt.Errorf("stack %q not found", args.Stack)
			}
			opts := network.ExecOpts{
				Cmd:     args.Cmd,
				Env:     args.Env,
				User:    args.User,
				WorkDir: args.WorkDir,
			}
			if args.TimeoutSec > 0 {
				opts.Timeout = time.Duration(args.TimeoutSec) * time.Second
			}
			if args.Stdin != "" {
				opts.Stdin = []byte(args.Stdin)
			}
			res, err := s.Exec(bc.Ctx, args.Service, args.Replica, opts)
			if err != nil {
				return execResult{}, err
			}
			return execResult{
				ExitCode:   res.ExitCode,
				Stdout:     string(res.Stdout),
				Stderr:     string(res.Stderr),
				DurationMs: res.Duration.Milliseconds(),
			}, nil
		})); err != nil {
		return err
	}

	if err := register("stack.address",
		runtime.JSON(func(bc runtime.BridgeCtx, args addressArgs) (string, error) {
			// For v1 we don't expose ingress addresses by service
			// (the stack's gateway has them, but JS doesn't usually
			// need them — it uses internal DNS). Return the
			// service alias so JS code can construct
			// "http://<name>:<port>" itself.
			return args.Service, nil
		})); err != nil {
		return err
	}

	if err := register("log",
		runtime.JSON(func(bc runtime.BridgeCtx, args logArgs) (struct{}, error) {
			// Logs to stderr for v1; replace with structured
			// emitter once the rest of mkfst settles on slog.
			fmt.Printf("[ts] %s %s %v\n", args.Level, args.Msg, args.Fields)
			return struct{}{}, nil
		})); err != nil {
		return err
	}

	return nil
}

// === arg / result types ===

type runOneShotArgs struct {
	Stack         string            `json:"stack"`
	Image         string            `json:"image"`
	Cmd           []string          `json:"cmd"`
	Entrypoint    []string          `json:"entrypoint"`
	Env           map[string]string `json:"env"`
	WorkDir       string            `json:"workDir"`
	User          string            `json:"user"`
	Stdin         string            `json:"stdin"`
	TimeoutSec    int               `json:"timeoutSec"`
	Aliases       []string          `json:"aliases"`
	PullIfMissing bool              `json:"pullIfMissing"`
}

type runOneShotResult struct {
	ContainerID string `json:"containerId"`
	ExitCode    int    `json:"exitCode"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	DurationMs  int64  `json:"durationMs"`
}

type execArgs struct {
	Stack      string            `json:"stack"`
	Service    string            `json:"service"`
	Replica    int               `json:"replica"`
	Cmd        []string          `json:"cmd"`
	Env        map[string]string `json:"env"`
	User       string            `json:"user"`
	WorkDir    string            `json:"workDir"`
	Stdin      string            `json:"stdin"`
	TimeoutSec int               `json:"timeoutSec"`
}

type execResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"durationMs"`
}

type addressArgs struct {
	Stack   string `json:"stack"`
	Service string `json:"service"`
}

type logArgs struct {
	Level  string                 `json:"level"`
	Msg    string                 `json:"msg"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// === MapStackResolver ===

// MapStackResolver is a simple StackResolver backed by a map.
type MapStackResolver struct {
	mu     sync.RWMutex
	stacks map[string]*network.Stack
}

// NewMapStackResolver returns an empty resolver.
func NewMapStackResolver() *MapStackResolver {
	return &MapStackResolver{stacks: map[string]*network.Stack{}}
}

// Set adds or replaces a stack mapping.
func (r *MapStackResolver) Set(name string, s *network.Stack) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stacks[name] = s
}

// Lookup is the StackResolver fn.
func (r *MapStackResolver) Lookup(name string) (*network.Stack, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.stacks[name]
	return s, ok
}

// silence unused-import shim
var _ = base64.StdEncoding
