// 12-stacks — API server managing a docker stack via providers/docker/network.
//
// Demonstrates:
//   - Bringing up a Compose-like multi-container stack from inside an
//     API process.
//   - Per-service health probes that gate readiness.
//   - In-process gateway with allow/deny rules + monitoring.
//   - HTTP endpoints to inspect status, list ingress addresses, and
//     stream events.
//
// Requires a reachable docker daemon (rootful or rootless).
//
// Run from the repo root:
//
//	DOCKER_HOST=unix:///run/user/$(id -u)/docker.sock \
//	  go run ./examples/12-stacks
//
// Then exercise:
//
//	curl http://localhost:8081/stack/status
//	curl http://localhost:8081/stack/ingress
//	curl http://$(curl -s http://localhost:8081/stack/ingress | jq -r .web)/  # hits nginx
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"mkfst/config"
	"mkfst/fizz"
	"mkfst/fizz/openapi"
	dockerprov "mkfst/providers/docker"
	"mkfst/providers/docker/network"
	"mkfst/service"
)

func main() {
	cli, err := dockerprov.New(dockerprov.Opts{Timeout: 5 * time.Second})
	if err != nil {
		if errors.Is(err, dockerprov.ErrUnreachable) {
			log.Fatalf("docker daemon unreachable; set DOCKER_HOST: %v", err)
		}
		log.Fatal(err)
	}
	defer cli.Close()

	netEng, err := network.NewEngine(cli.SDK(), network.EngineOpts{})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Define a 2-service stack: nginx on :80 + redis on :6379.
	stack, err := netEng.NewStack("demo-stack")
	if err != nil {
		log.Fatal(err)
	}
	stack.MustAddService("web",
		network.Image("nginx:alpine"),
		network.Port(80),
		network.WithProbe(
			network.HTTPProbe(80, "/").WithFailureThreshold(40),
			network.ProbeReadiness,
		),
	)
	stack.MustAddService("cache",
		network.Image("redis:7-alpine"),
		network.Port(6379),
		network.WithProbe(network.TCPProbe(6379), network.ProbeReadiness),
	)

	// Expose web on a host-side gateway with a /16 allow rule.
	webIngress, err := stack.Ingress("web-in", "web", 80,
		network.AllowSource("127.0.0.0/8"),
	)
	if err != nil {
		log.Fatal(err)
	}

	upCtx, upCancel := context.WithTimeout(ctx, 90*time.Second)
	if err := stack.Up(upCtx); err != nil {
		upCancel()
		log.Fatalf("stack up: %v", err)
	}
	upCancel()
	log.Printf("stack up: web ingress at %s", webIngress.Address())

	// Drain monitor events to stdout for visibility.
	mon := stack.Monitor()
	go func() {
		for ev := range mon.Events() {
			log.Printf("[monitor] kind=%d service=%s reason=%s",
				ev.Kind, ev.Service, ev.DenyReason+ev.Error)
		}
	}()

	// Clean shutdown on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutdown signal — taking stack down")
		dc, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = stack.Down(dc)
		_ = netEng.Close(dc)
		os.Exit(0)
	}()

	svc := service.Create(config.Config{
		Host: "localhost", Port: 8081, SkipDB: true,
		Spec: openapi.Info{Title: "Stacks Demo", Version: "v1.0.0"},
	})

	svc.Route("GET", "/stack/status", 200,
		[]fizz.OperationOption{fizz.Summary("Stack + service status")},
		func(g *gin.Context, _ *sql.DB) (network.StackStatus, error) {
			return stack.Status(g.Request.Context()), nil
		},
	)

	svc.Route("GET", "/stack/ingress", 200, nil,
		func(g *gin.Context, _ *sql.DB) (map[string]string, error) {
			return map[string]string{"web": webIngress.Address()}, nil
		},
	)

	// Spawn a one-shot test container against the stack.
	svc.Route("POST", "/stack/oneshot/echo", 200, nil,
		func(g *gin.Context, _ *sql.DB, in *struct {
			Msg string `json:"msg" validate:"required"`
		}) (struct{ Output string }, error) {
			res, err := stack.RunOneShot(g.Request.Context(), network.OneShotOpts{
				Image:   "alpine:3.19",
				Cmd:     []string{"sh", "-c", fmt.Sprintf("printf '%%s\\n' %q", in.Msg)},
				Timeout: 30 * time.Second,
			})
			if err != nil {
				return struct{ Output string }{}, err
			}
			return struct{ Output string }{Output: string(res.Stdout)}, nil
		},
	)

	// Exec something inside an existing service.
	svc.Route("POST", "/stack/exec/redis-ping", 200, nil,
		func(g *gin.Context, _ *sql.DB) (struct{ Reply string }, error) {
			r, err := stack.Exec(g.Request.Context(), "cache", 0, network.ExecOpts{
				Cmd: []string{"redis-cli", "PING"},
			})
			if err != nil {
				return struct{ Reply string }{}, err
			}
			return struct{ Reply string }{Reply: string(r.Stdout)}, nil
		},
	)

	svc.Run()
}
