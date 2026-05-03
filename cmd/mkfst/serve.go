package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	dockerprov "mkfst/providers/docker"
	"mkfst/providers/docker/network"
	"mkfst/providers/tasks"
	"mkfst/providers/ts"
	"mkfst/providers/ts/bundle"
	tsconfig "mkfst/providers/ts/config"
	tsruntime "mkfst/providers/ts/runtime"
	tsserver "mkfst/providers/ts/server"
	"mkfst/providers/workflows"
)

// runServe is invoked by `mkfst serve`.
func runServe(args []string) {
	configPath := ""
	listen := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			i++
			if i < len(args) {
				configPath = args[i]
			}
		case "--listen":
			i++
			if i < len(args) {
				listen = args[i]
			}
		}
	}
	if configPath == "" {
		// Search a few conventional spots.
		for _, p := range []string{"mkfst.yaml", "/etc/mkfst/mkfst.yaml"} {
			if _, err := os.Stat(p); err == nil {
				configPath = p
				break
			}
		}
	}
	if configPath == "" {
		fatal("serve: no --config supplied and no mkfst.yaml found")
	}

	cfg, err := tsconfig.Load(configPath)
	if err != nil {
		fatal("serve: load config: " + err.Error())
	}
	if listen != "" {
		cfg.Server.Listen = listen
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8443"
	}

	// 1. Docker + network engine.
	cli, err := dockerprov.New(dockerprov.Opts{})
	if err != nil {
		fatal("serve: docker: " + err.Error())
	}
	netEng, err := network.NewEngine(cli.SDK(), network.EngineOpts{})
	if err != nil {
		fatal("serve: network engine: " + err.Error())
	}

	// 2. Apply stacks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := tsconfig.ApplyStacks(ctx, cfg, netEng); err != nil {
		fatal("serve: apply stacks: " + err.Error())
	}

	// 3. Build allowlist from config.
	al := bundle.NewAllowlist(cfg.Modules.Cache)
	for _, name := range cfg.Modules.Allow {
		root := stripVersion(name)
		path := filepath.Join(cfg.Modules.Cache, root)
		if err := al.Add(bundle.ModuleEntry{Name: root, Path: path}); err != nil {
			fatal("serve: add module " + name + ": " + err.Error())
		}
	}

	// 4. Capability registry — load every approved module's
	//    package.json and register its declared capabilities.
	capReg := tsruntime.NewCapabilityRegistry()
	for _, name := range cfg.Modules.Allow {
		root := stripVersion(name)
		pkgPath := filepath.Join(cfg.Modules.Cache, root, "package.json")
		raw, err := os.ReadFile(pkgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: missing package.json for %s: %v\n", root, err)
			continue
		}
		mc, err := tsruntime.LoadFromPackageJSON(root, raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: capabilities for %s: %v\n", root, err)
			continue
		}
		capReg.Add(mc)
	}
	capReg.SetOperatorOverride(cfg.Capabilities)

	// 5. Workflow engine + worker.
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	worker, err := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 8,
		PollInterval: 25 * time.Millisecond, MaintenanceInterval: 100 * time.Millisecond,
	})
	if err != nil {
		fatal("serve: worker: " + err.Error())
	}
	wfEng, err := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
	})
	if err != nil {
		fatal("serve: workflow engine: " + err.Error())
	}
	go func() { _ = worker.Run(ctx) }()

	// 6. Bridge + stack resolver.
	resolver := ts.NewMapStackResolver()
	for _, s := range netEng.Stacks() {
		resolver.Set(s.Name(), s)
	}
	bridge := tsruntime.NewBridge(capReg)
	if err := ts.RegisterStackHandlers(bridge, resolver.Lookup); err != nil {
		fatal("serve: register handlers: " + err.Error())
	}

	// 7. TS engine.
	tsEng, err := ts.NewEngine(ts.EngineOpts{
		WorkflowEngine: wfEng,
		Allowlist:      al,
		Bridge:         bridge,
		EmitSourceMaps: true,
	})
	if err != nil {
		fatal("serve: ts engine: " + err.Error())
	}

	// 8. HTTP server.
	srv := tsserver.NewServer(tsEng)
	srv.SetStackLister(func() []tsserver.StackInfo {
		out := []tsserver.StackInfo{}
		for _, s := range netEng.Stacks() {
			st := tsserver.StackInfo{
				Name:     s.Name(),
				State:    s.State().String(),
				Services: map[string]tsserver.ServiceInfo{},
			}
			status := s.Status(ctx)
			for n, svc := range status.Services {
				st.Services[n] = tsserver.ServiceInfo{
					Image:    svc.Image,
					Replicas: svc.Replicas,
					Healthy:  svc.Healthy,
				}
			}
			out = append(out, st)
		}
		return out
	})
	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 9. mTLS if configured.
	if cfg.Server.TLS.Cert != "" && cfg.Server.TLS.Key != "" {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		// Optional client CA for mTLS — when present, require
		// client certificates verified against it.
		if caPath := os.Getenv("MKFST_CLIENT_CA"); caPath != "" {
			caBytes, err := os.ReadFile(caPath)
			if err != nil {
				fatal("serve: read client CA: " + err.Error())
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caBytes) {
				fatal("serve: parse client CA")
			}
			tlsCfg.ClientCAs = pool
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
		httpSrv.TLSConfig = tlsCfg
		fmt.Printf("mkfst: listening on https://%s (mTLS=%v)\n",
			cfg.Server.Listen, tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert)
		go func() {
			if err := httpSrv.ListenAndServeTLS(cfg.Server.TLS.Cert, cfg.Server.TLS.Key); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "serve: ListenAndServeTLS:", err)
				os.Exit(1)
			}
		}()
	} else {
		fmt.Printf("mkfst: listening on http://%s\n", cfg.Server.Listen)
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "serve: ListenAndServe:", err)
				os.Exit(1)
			}
		}()
	}

	// 10. Wait for signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Fprintln(os.Stderr, "mkfst: shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	for _, s := range netEng.Stacks() {
		_ = s.Down(shutdownCtx)
	}
	_ = netEng.Close(shutdownCtx)
}

// === stack subcommands ===

func runStackApply(args []string) {
	configPath := "mkfst.yaml"
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	cfg, err := tsconfig.Load(configPath)
	if err != nil {
		fatal("stack apply: " + err.Error())
	}
	cli, err := dockerprov.New(dockerprov.Opts{})
	if err != nil {
		fatal("stack apply: docker: " + err.Error())
	}
	netEng, err := network.NewEngine(cli.SDK(), network.EngineOpts{})
	if err != nil {
		fatal("stack apply: network engine: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stacks, err := tsconfig.ApplyStacks(ctx, cfg, netEng)
	if err != nil {
		fatal("stack apply: " + err.Error())
	}
	for _, s := range stacks {
		fmt.Printf("stack %s applied (state=%s)\n", s.Name(), s.State())
	}
}

func runStackList(args []string) {
	server := defaultServerURL()
	for i := 0; i < len(args); i++ {
		if args[i] == "--server" && i+1 < len(args) {
			server = args[i+1]
		}
	}
	resp, err := http.Get(server + "/v1/stacks")
	if err != nil {
		fatal("stack list: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintln(os.Stderr, "server doesn't expose /v1/stacks (older build)")
		os.Exit(1)
	}
	dec := json.NewDecoder(resp.Body)
	var v map[string]any
	_ = dec.Decode(&v)
	pp, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(pp))
}

// === module subcommands ===

func runModuleAdd(args []string) {
	if len(args) < 1 {
		fatal("usage: mkfst module add NAME[@VERSION] [--cache DIR]")
	}
	name := stripVersion(args[0])
	cache := "."
	for i := 1; i < len(args); i++ {
		if args[i] == "--cache" && i+1 < len(args) {
			cache = args[i+1]
		}
	}
	target := filepath.Join(cache, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		fatal("module add: " + err.Error())
	}
	pkgPath := filepath.Join(target, "package.json")
	if _, err := os.Stat(pkgPath); err == nil {
		fmt.Printf("module %s already in cache at %s\n", name, target)
		return
	}
	// Minimal v1: create a stub package.json so operators can drop
	// in module sources alongside. Real registry-fetch is left to
	// `npm install --prefix` invocation by the operator; we
	// deliberately don't reach out to the network.
	stub := map[string]any{
		"name":    name,
		"version": "0.0.0-vendored",
		"main":    "src/index.ts",
		"types":   "src/index.ts",
		"mkfst":   map[string]any{"capabilities": map[string]any{}},
	}
	b, _ := json.MarshalIndent(stub, "", "  ")
	if err := os.WriteFile(pkgPath, b, 0o644); err != nil {
		fatal("module add: write stub: " + err.Error())
	}
	fmt.Printf("module %s added at %s (drop sources under src/ to enable)\n", name, target)
}

func runModuleList(args []string) {
	cache := "."
	for i := 0; i < len(args); i++ {
		if args[i] == "--cache" && i+1 < len(args) {
			cache = args[i+1]
		}
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		fatal("module list: " + err.Error())
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := filepath.Join(cache, e.Name(), "package.json")
		if _, err := os.Stat(pkg); err == nil {
			fmt.Println(e.Name())
		}
	}
}

// stripVersion drops "@1.0.0" from a "name@version" specifier.
// Safe for scoped packages: "@scope/name@1.0" → "@scope/name".
func stripVersion(spec string) string {
	at := -1
	for i := 0; i < len(spec); i++ {
		if spec[i] == '@' && i > 0 {
			at = i
			break
		}
	}
	if at < 0 {
		return spec
	}
	return spec[:at]
}
