package docker

import (
	"reflect"
	"testing"
	"time"
)

func TestRunOptionsAssembleConfigAndHostConfig(t *testing.T) {
	state := newRunState("alpine:3.19")
	for _, opt := range []RunOption{
		Name("my-svc"),
		Cmd("echo", "hello"),
		Env("FOO", "bar"),
		Env("BAZ", "qux"),
		WorkDir("/app"),
		User("1000:1000"),
		Hostname("svc1"),
		Port(PortMap{ContainerPort: 80, HostPort: 8080}),
		Port(PortMap{ContainerPort: 443, Protocol: "tcp"}),
		Mount(MountSpec{Type: MountTypeBind, Source: "/host/data", Target: "/data", ReadOnly: true}),
		Tmpfs("/run", ""),
		AutoRemove(),
		Privileged(),
		ReadonlyRootfs(),
		MemoryLimit(512 * 1024 * 1024),
		CPUs(2.5),
		CapAdd("NET_ADMIN"),
		CapDrop("MKNOD"),
		Restart(RestartPolicy{Name: "on-failure", MaxRetries: 5}),
		Network("host"),
		AddHost("ci.local:10.0.0.1"),
		DNS("1.1.1.1"),
		Sysctl("net.core.somaxconn", "1024"),
		StopSignal("SIGINT"),
		StopTimeout(15),
		UseInit(),
		HealthProbe(HealthCheck{
			Test:     []string{"CMD-SHELL", "curl -f localhost/healthz"},
			Interval: 10 * time.Second,
			Timeout:  3 * time.Second,
			Retries:  3,
		}),
	} {
		opt(state)
	}

	if state.name != "my-svc" {
		t.Fatalf("Name: %q", state.name)
	}
	if !reflect.DeepEqual([]string(state.config.Cmd), []string{"echo", "hello"}) {
		t.Fatalf("Cmd: %v", state.config.Cmd)
	}
	if !reflect.DeepEqual(state.config.Env, []string{"FOO=bar", "BAZ=qux"}) {
		t.Fatalf("Env: %v", state.config.Env)
	}
	if state.config.WorkingDir != "/app" {
		t.Fatalf("WorkingDir: %q", state.config.WorkingDir)
	}
	if state.config.User != "1000:1000" {
		t.Fatalf("User: %q", state.config.User)
	}
	if state.config.Hostname != "svc1" {
		t.Fatalf("Hostname: %q", state.config.Hostname)
	}

	if len(state.host.PortBindings) != 2 {
		t.Fatalf("PortBindings: %+v", state.host.PortBindings)
	}
	if len(state.host.Mounts) != 1 || state.host.Mounts[0].Source != "/host/data" {
		t.Fatalf("Mounts: %+v", state.host.Mounts)
	}
	if state.host.Tmpfs == nil || state.host.Tmpfs["/run"] != "" {
		t.Fatalf("Tmpfs: %+v", state.host.Tmpfs)
	}
	if !state.host.AutoRemove {
		t.Fatalf("AutoRemove not set")
	}
	if !state.host.Privileged {
		t.Fatalf("Privileged not set")
	}
	if !state.host.ReadonlyRootfs {
		t.Fatalf("ReadonlyRootfs not set")
	}
	if state.host.Memory != 512*1024*1024 {
		t.Fatalf("Memory: %d", state.host.Memory)
	}
	if state.host.NanoCPUs != int64(2.5*1e9) {
		t.Fatalf("NanoCPUs: %d", state.host.NanoCPUs)
	}
	if !reflect.DeepEqual([]string(state.host.CapAdd), []string{"NET_ADMIN"}) {
		t.Fatalf("CapAdd: %v", state.host.CapAdd)
	}
	if !reflect.DeepEqual([]string(state.host.CapDrop), []string{"MKNOD"}) {
		t.Fatalf("CapDrop: %v", state.host.CapDrop)
	}
	if string(state.host.RestartPolicy.Name) != "on-failure" || state.host.RestartPolicy.MaximumRetryCount != 5 {
		t.Fatalf("RestartPolicy: %+v", state.host.RestartPolicy)
	}
	if string(state.host.NetworkMode) != "host" {
		t.Fatalf("NetworkMode: %q", state.host.NetworkMode)
	}
	if state.config.StopSignal != "SIGINT" {
		t.Fatalf("StopSignal: %q", state.config.StopSignal)
	}
	if state.config.StopTimeout == nil || *state.config.StopTimeout != 15 {
		t.Fatalf("StopTimeout: %v", state.config.StopTimeout)
	}
	if state.host.Init == nil || !*state.host.Init {
		t.Fatalf("Init: %v", state.host.Init)
	}
	if state.config.Healthcheck == nil || state.config.Healthcheck.Interval != 10*time.Second {
		t.Fatalf("Healthcheck: %+v", state.config.Healthcheck)
	}
}

func TestPortDefaultsProtocolToTCP(t *testing.T) {
	state := newRunState("img")
	Port(PortMap{ContainerPort: 80})(state)
	bindings := state.host.PortBindings
	if len(bindings) != 1 {
		t.Fatalf("PortBindings: %+v", bindings)
	}
	for k := range bindings {
		if k.Proto() != "tcp" {
			t.Fatalf("default protocol should be tcp, got %s", k.Proto())
		}
	}
}

func TestDetachAndWaitForExitAreInverse(t *testing.T) {
	state := newRunState("img")
	if !state.detach {
		t.Fatalf("default should be detach=true")
	}
	WaitForExit()(state)
	if state.detach {
		t.Fatalf("WaitForExit should clear detach")
	}
	Detach()(state)
	if !state.detach {
		t.Fatalf("Detach should set detach=true")
	}
}

func TestParsePlatformSplitsTriple(t *testing.T) {
	cases := []struct {
		in           string
		os, arch, v  string
	}{
		{"linux/amd64", "linux", "amd64", ""},
		{"linux/arm64", "linux", "arm64", ""},
		{"linux/arm/v7", "linux", "arm", "v7"},
		{"darwin/arm64", "darwin", "arm64", ""},
	}
	for _, c := range cases {
		os, arch, v := parsePlatform(c.in)
		if os != c.os || arch != c.arch || v != c.v {
			t.Fatalf("parsePlatform(%q) = (%q,%q,%q) want (%q,%q,%q)",
				c.in, os, arch, v, c.os, c.arch, c.v)
		}
	}
}
