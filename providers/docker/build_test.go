package docker

import (
	"reflect"
	"testing"
)

func TestBuildOptionsApplyInOrder(t *testing.T) {
	state := &buildState{}
	for _, opt := range []BuildOption{
		Tag("img:v1"),
		Tag("img:latest"),
		Arg("VERSION", "1.0"),
		Arg("BUILD_ID", "abc"),
		Label("owner", "team-x"),
		Target("prod"),
		BuildPlatform("linux/amd64"),
		NoCache(),
		Pull(),
		ShmSize(128 << 20),
		CacheFrom("img:cache-a", "img:cache-b"),
		Ulimit("nofile", 1024, 4096),
		ExtraHost("ci.local:10.0.0.1"),
		BuildNetwork("host"),
	} {
		opt(state)
	}

	if !reflect.DeepEqual(state.opts.Tags, []string{"img:v1", "img:latest"}) {
		t.Fatalf("Tags: %v", state.opts.Tags)
	}
	if state.opts.BuildArgs == nil ||
		state.opts.BuildArgs["VERSION"] == nil || *state.opts.BuildArgs["VERSION"] != "1.0" ||
		state.opts.BuildArgs["BUILD_ID"] == nil || *state.opts.BuildArgs["BUILD_ID"] != "abc" {
		t.Fatalf("BuildArgs: %+v", state.opts.BuildArgs)
	}
	if state.opts.Labels["owner"] != "team-x" {
		t.Fatalf("Labels: %+v", state.opts.Labels)
	}
	if state.opts.Target != "prod" {
		t.Fatalf("Target: %q", state.opts.Target)
	}
	if state.opts.Platform != "linux/amd64" {
		t.Fatalf("Platform: %q", state.opts.Platform)
	}
	if !state.opts.NoCache {
		t.Fatalf("NoCache not set")
	}
	if !state.opts.PullParent {
		t.Fatalf("PullParent not set")
	}
	if state.opts.ShmSize != 128<<20 {
		t.Fatalf("ShmSize: %d", state.opts.ShmSize)
	}
	if !reflect.DeepEqual(state.opts.CacheFrom, []string{"img:cache-a", "img:cache-b"}) {
		t.Fatalf("CacheFrom: %v", state.opts.CacheFrom)
	}
	if len(state.opts.Ulimits) != 1 || state.opts.Ulimits[0].Name != "nofile" {
		t.Fatalf("Ulimits: %+v", state.opts.Ulimits)
	}
	if !reflect.DeepEqual(state.opts.ExtraHosts, []string{"ci.local:10.0.0.1"}) {
		t.Fatalf("ExtraHosts: %v", state.opts.ExtraHosts)
	}
	if state.opts.NetworkMode != "host" {
		t.Fatalf("NetworkMode: %q", state.opts.NetworkMode)
	}
}

func TestArgFromEnvSetsNilValue(t *testing.T) {
	state := &buildState{}
	ArgFromEnv("HTTP_PROXY")(state)
	if state.opts.BuildArgs == nil {
		t.Fatalf("BuildArgs not initialized")
	}
	v, ok := state.opts.BuildArgs["HTTP_PROXY"]
	if !ok {
		t.Fatalf("HTTP_PROXY not set")
	}
	if v != nil {
		t.Fatalf("ArgFromEnv should set nil value to inherit from daemon env, got %v", v)
	}
}

func TestRegistryAuthEncodes(t *testing.T) {
	state := &buildState{}
	RegistryAuth("ghcr.io", AuthConfig{
		Username: "u",
		Password: "p",
	})(state)
	if state.opts.AuthConfigs == nil {
		t.Fatalf("AuthConfigs not initialized")
	}
	got := state.opts.AuthConfigs["ghcr.io"]
	if got.Username != "u" || got.Password != "p" {
		t.Fatalf("AuthConfig content: %+v", got)
	}
}

func TestKeepIntermediateOverridesDefault(t *testing.T) {
	// Replicate the default initialization Client.Build does. Remove=true
	// is the docker CLI default; KeepIntermediate flips it.
	state := &buildState{}
	state.opts.Remove = true
	KeepIntermediate()(state)
	if state.opts.Remove {
		t.Fatalf("KeepIntermediate should set Remove=false")
	}
}
