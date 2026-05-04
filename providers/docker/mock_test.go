package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/mock"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// === Run / lifecycle path ===

func TestRun_ContainerCreateErrorNoStart(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, "svc").
		Return(container.CreateResponse{}, errors.New("create boom")).
		Once()

	res, err := c.Run(context.Background(), "alpine:3.19", Name("svc"))
	if err == nil {
		t.Fatal("expected error")
	}
	if res != nil {
		t.Fatalf("unexpected res: %+v", res)
	}
	if !strings.Contains(err.Error(), "create boom") {
		t.Fatalf("error chain doesn't wrap original: %v", err)
	}
}

func TestRun_StartFailureRemovesContainer(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(container.CreateResponse{ID: "abc"}, nil).
		Once()
	api.EXPECT().
		ContainerStart(mock.Anything, "abc", mock.Anything).
		Return(errors.New("start fail")).
		Once()
	// On start failure the wrapper does best-effort cleanup.
	api.EXPECT().
		ContainerRemove(mock.Anything, "abc", mock.MatchedBy(func(opts container.RemoveOptions) bool {
			return opts.Force
		})).
		Return(nil).
		Once()

	if _, err := c.Run(context.Background(), "alpine"); err == nil {
		t.Fatal("expected start error")
	}
}

func TestRun_PrestartHookFailureCleansContainer(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(container.CreateResponse{ID: "abc"}, nil).
		Once()
	api.EXPECT().
		ContainerRemove(mock.Anything, "abc", mock.Anything).
		Return(nil).
		Once()

	hookFired := false
	prestart := func(s *runState) {
		s.prestart = append(s.prestart, func(ctx context.Context, c *Client, id string) error {
			hookFired = true
			return errors.New("hook fail")
		})
	}
	if _, err := c.Run(context.Background(), "alpine", prestart); err == nil {
		t.Fatal("expected prestart error")
	}
	if !hookFired {
		t.Fatal("prestart hook not invoked")
	}
}

func TestRun_PoststartHookFailureCleansContainer(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(container.CreateResponse{ID: "xyz"}, nil).
		Once()
	api.EXPECT().
		ContainerStart(mock.Anything, "xyz", mock.Anything).
		Return(nil).
		Once()
	api.EXPECT().
		ContainerRemove(mock.Anything, "xyz", mock.Anything).
		Return(nil).
		Once()

	poststart := func(s *runState) {
		s.poststart = append(s.poststart, func(ctx context.Context, c *Client, id string) error {
			return errors.New("post fail")
		})
	}
	if _, err := c.Run(context.Background(), "alpine", poststart); err == nil {
		t.Fatal("expected poststart error")
	}
}

func TestRun_DetachReturnsWithoutWait(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(container.CreateResponse{ID: "abc"}, nil).
		Once()
	api.EXPECT().
		ContainerStart(mock.Anything, "abc", mock.Anything).
		Return(nil).
		Once()

	res, err := c.Run(context.Background(), "alpine") // detach is the default
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.ContainerID != "abc" {
		t.Fatalf("ID: %q", res.ContainerID)
	}
	// ContainerWait must NOT be called — testify will fail the test
	// at cleanup if it was set up but not invoked, and equally if it
	// was called without an expectation.
}

func TestRun_WaitForExitCapturesExitCode(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(container.CreateResponse{ID: "abc"}, nil).
		Once()
	api.EXPECT().
		ContainerStart(mock.Anything, "abc", mock.Anything).
		Return(nil).
		Once()

	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{StatusCode: 42}
	errCh := make(chan error, 1)
	api.EXPECT().
		ContainerWait(mock.Anything, "abc", container.WaitConditionNotRunning).
		Return((<-chan container.WaitResponse)(statusCh), (<-chan error)(errCh)).
		Once()

	res, err := c.Run(context.Background(), "alpine", WaitForExit())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("ExitCode: %d", res.ExitCode)
	}
}

func TestRun_OptionTranslationReachesSDK(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	var (
		gotConfig   *container.Config
		gotHost     *container.HostConfig
		gotNet      *network.NetworkingConfig
		gotPlatform *ocispec.Platform
		gotName     string
	)

	api.EXPECT().
		ContainerCreate(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(_ context.Context, cfg *container.Config, host *container.HostConfig,
			net *network.NetworkingConfig, plat *ocispec.Platform, name string) {
			gotConfig = cfg
			gotHost = host
			gotNet = net
			gotPlatform = plat
			gotName = name
		}).
		Return(container.CreateResponse{ID: "x"}, nil).
		Once()
	api.EXPECT().ContainerStart(mock.Anything, "x", mock.Anything).Return(nil).Once()

	_, err := c.Run(context.Background(), "alpine:3.19",
		Name("svc"),
		Cmd("echo", "hi"),
		Env("FOO", "bar"),
		Mount(MountSpec{Type: MountTypeBind, Source: "/h", Target: "/c"}),
		AttachNetwork("frontend"),
		RunPlatform("linux/arm64"),
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if gotName != "svc" {
		t.Fatalf("name: %q", gotName)
	}
	if gotConfig.Image != "alpine:3.19" {
		t.Fatalf("image: %q", gotConfig.Image)
	}
	if len(gotConfig.Cmd) != 2 || gotConfig.Cmd[0] != "echo" {
		t.Fatalf("cmd: %v", gotConfig.Cmd)
	}
	if len(gotConfig.Env) != 1 || gotConfig.Env[0] != "FOO=bar" {
		t.Fatalf("env: %v", gotConfig.Env)
	}
	if len(gotHost.Mounts) != 1 || gotHost.Mounts[0].Source != "/h" {
		t.Fatalf("mounts: %+v", gotHost.Mounts)
	}
	if _, ok := gotNet.EndpointsConfig["frontend"]; !ok {
		t.Fatalf("network attach missing: %+v", gotNet.EndpointsConfig)
	}
	if gotPlatform == nil || gotPlatform.Architecture != "arm64" {
		t.Fatalf("platform: %+v", gotPlatform)
	}
}

func TestStop_PassesTimeout(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerStop(mock.Anything, "abc", mock.MatchedBy(func(opts container.StopOptions) bool {
			return opts.Timeout != nil && *opts.Timeout == 15
		})).
		Return(nil).
		Once()

	d := 15 * time.Second
	if err := c.Stop(context.Background(), "abc", &d); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestKill_PassesSignal(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().ContainerKill(mock.Anything, "abc", "SIGUSR1").Return(nil).Once()

	if err := c.Kill(context.Background(), "abc", "SIGUSR1"); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRestart_PassesTimeout(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerRestart(mock.Anything, "abc", mock.MatchedBy(func(opts container.StopOptions) bool {
			return opts.Timeout != nil && *opts.Timeout == 5
		})).
		Return(nil).
		Once()

	d := 5 * time.Second
	if err := c.Restart(context.Background(), "abc", &d); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestRemove_PassesOptions(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerRemove(mock.Anything, "abc", mock.MatchedBy(func(opts container.RemoveOptions) bool {
			return opts.Force && opts.RemoveVolumes && !opts.RemoveLinks
		})).
		Return(nil).
		Once()

	if err := c.Remove(context.Background(), "abc", RemoveOpts{Force: true, RemoveVolumes: true}); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestWait_TranslatesCondition(t *testing.T) {
	cases := []struct {
		in   string
		want container.WaitCondition
	}{
		{"", container.WaitConditionNotRunning},
		{"not-running", container.WaitConditionNotRunning},
		{"next-exit", container.WaitConditionNextExit},
		{"removed", container.WaitConditionRemoved},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			api := newMockdockerAPI(t)
			c := fromAPI(api)

			statusCh := make(chan container.WaitResponse, 1)
			statusCh <- container.WaitResponse{StatusCode: 7}
			errCh := make(chan error, 1)
			api.EXPECT().
				ContainerWait(mock.Anything, "abc", tc.want).
				Return((<-chan container.WaitResponse)(statusCh), (<-chan error)(errCh)).
				Once()

			code, err := c.Wait(context.Background(), "abc", tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if code != 7 {
				t.Fatalf("code: %d", code)
			}
		})
	}
}

func TestWait_StatusErrorIsWrapped(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	statusCh := make(chan container.WaitResponse, 1)
	statusCh <- container.WaitResponse{
		StatusCode: 137,
		Error:      &container.WaitExitError{Message: "oom"},
	}
	errCh := make(chan error, 1)
	api.EXPECT().
		ContainerWait(mock.Anything, "abc", mock.Anything).
		Return((<-chan container.WaitResponse)(statusCh), (<-chan error)(errCh)).
		Once()

	code, err := c.Wait(context.Background(), "abc", "")
	if code != 137 {
		t.Fatalf("code: %d", code)
	}
	if err == nil || !strings.Contains(err.Error(), "oom") {
		t.Fatalf("expected oom error, got %v", err)
	}
}

func TestInspect_ReturnsSDKShape(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	resp := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: "abc", Image: "alpine"},
	}
	api.EXPECT().ContainerInspect(mock.Anything, "abc").Return(resp, nil).Once()

	got, err := c.Inspect(context.Background(), "abc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.ID != "abc" || got.Image != "alpine" {
		t.Fatalf("inspect: %+v", got)
	}
}

func TestList_PassesOptions(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerList(mock.Anything, mock.MatchedBy(func(opts container.ListOptions) bool {
			return opts.All && opts.Limit == 50
		})).
		Return([]container.Summary{{ID: "a"}, {ID: "b"}}, nil).
		Once()

	got, err := c.List(context.Background(), WithListAll(), WithListLimit(50))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: %d", len(got))
	}
}

// === Pull / Build streaming ===

func TestPull_StreamsEvents(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	body := io.NopCloser(strings.NewReader(
		`{"id":"layer1","status":"Pulling fs layer"}` + "\n" +
			`{"aux":{"ID":"sha256:final"}}` + "\n",
	))
	api.EXPECT().
		ImagePull(mock.Anything, "alpine:3.19", mock.MatchedBy(func(opts dockerimage.PullOptions) bool {
			return opts.Platform == "linux/arm64"
		})).
		Return(body, nil).
		Once()

	events, err := c.Pull(context.Background(), "alpine:3.19", PullPlatform("linux/arm64"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	collected := drain(events)
	// Expect: status, aux, done
	if len(collected) < 3 {
		t.Fatalf("event count: %d (%+v)", len(collected), collected)
	}
	if collected[0].Kind != EventStatus {
		t.Fatalf("first kind: %s", collected[0].Kind)
	}
	if collected[1].Kind != EventAux || collected[1].ImageID() != "sha256:final" {
		t.Fatalf("aux: %+v", collected[1])
	}
	if collected[len(collected)-1].Kind != EventDone {
		t.Fatalf("terminator: %s", collected[len(collected)-1].Kind)
	}
}

func TestPull_SDKErrorBubbles(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ImagePull(mock.Anything, "bad:tag", mock.Anything).
		Return(nil, errors.New("manifest not found")).
		Once()

	if _, err := c.Pull(context.Background(), "bad:tag"); err == nil {
		t.Fatal("expected pull error")
	}
}

func TestBuild_StreamsEventsAndPassesOptions(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	body := io.NopCloser(strings.NewReader(
		`{"stream":"Step 1/2 : FROM scratch\n"}` + "\n" +
			`{"aux":{"ID":"sha256:built"}}` + "\n",
	))
	var capturedOpts build.ImageBuildOptions
	api.EXPECT().
		ImageBuild(mock.Anything, mock.Anything, mock.Anything).
		Run(func(_ context.Context, _ io.Reader, opts build.ImageBuildOptions) {
			capturedOpts = opts
		}).
		Return(build.ImageBuildResponse{Body: body}, nil).
		Once()

	src := &fakeSource{tar: io.NopCloser(strings.NewReader("tarbytes"))}
	events, err := c.Build(context.Background(), src, Tag("img:v1"), NoCache(), Target("prod"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(capturedOpts.Tags) != 1 || capturedOpts.Tags[0] != "img:v1" {
		t.Fatalf("tags not propagated: %v", capturedOpts.Tags)
	}
	if !capturedOpts.NoCache {
		t.Fatal("NoCache not propagated")
	}
	if capturedOpts.Target != "prod" {
		t.Fatalf("Target: %q", capturedOpts.Target)
	}

	collected := drain(events)
	if len(collected) < 3 {
		t.Fatalf("event count: %d (%+v)", len(collected), collected)
	}
	if collected[0].Kind != EventStream {
		t.Fatalf("first event kind: %+v", collected[0])
	}
	if collected[1].Kind != EventAux || collected[1].ImageID() != "sha256:built" {
		t.Fatalf("aux: %+v", collected[1])
	}
}

func TestBuild_NilSourceErrors(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)
	if _, err := c.Build(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil source")
	}
	// No SDK calls expected (mock asserts on cleanup).
}

func TestBuild_SourceTarErrorBubbles(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)
	src := &fakeSource{err: errors.New("tar pack failed")}
	if _, err := c.Build(context.Background(), src); err == nil {
		t.Fatal("expected tar error")
	}
}

func TestBuild_SDKErrorClosesTar(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	tarReader := &countingCloser{Reader: strings.NewReader("tarbytes")}
	src := &fakeSource{tar: tarReader}
	api.EXPECT().
		ImageBuild(mock.Anything, mock.Anything, mock.Anything).
		Return(build.ImageBuildResponse{}, errors.New("daemon down")).
		Once()

	if _, err := c.Build(context.Background(), src); err == nil {
		t.Fatal("expected error")
	}
	if tarReader.closed != 1 {
		t.Fatalf("tar should be closed on SDK error, closed=%d", tarReader.closed)
	}
}

func TestCancelBuild_PassesID(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().BuildCancel(mock.Anything, "build-42").Return(nil).Once()
	if err := c.CancelBuild(context.Background(), "build-42"); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestCancelBuild_WrapsError(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().BuildCancel(mock.Anything, "build-42").Return(errors.New("not found")).Once()
	err := c.CancelBuild(context.Background(), "build-42")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err: %v", err)
	}
}

// === Logs frame parsing (stdcopy demux) ===

func TestLogs_DemuxesStdoutAndStderrFrames(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	// stdcopy frame: 8-byte header [stream, 0,0,0, sz_be32], then payload.
	// stream: 1=stdout, 2=stderr.
	var buf bytes.Buffer
	writeStdcopyFrame(&buf, 1, "out-line-1\n")
	writeStdcopyFrame(&buf, 2, "err-line-1\n")
	writeStdcopyFrame(&buf, 1, "out-line-2\n")

	api.EXPECT().
		ContainerLogs(mock.Anything, "abc", mock.MatchedBy(func(opts container.LogsOptions) bool {
			return opts.ShowStdout && opts.ShowStderr
		})).
		Return(io.NopCloser(&buf), nil).
		Once()

	stream, err := c.Logs(context.Background(), "abc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	var stdoutLines, stderrLines []string
	for line := range stream {
		if line.Err != nil {
			t.Fatalf("stream error: %v", line.Err)
		}
		switch line.Stream {
		case LogStreamStdout:
			stdoutLines = append(stdoutLines, line.Message)
		case LogStreamStderr:
			stderrLines = append(stderrLines, line.Message)
		}
	}
	if len(stdoutLines) != 2 {
		t.Fatalf("stdout lines: %v", stdoutLines)
	}
	if len(stderrLines) != 1 || stderrLines[0] != "err-line-1" {
		t.Fatalf("stderr lines: %v", stderrLines)
	}
}

func TestLogs_FollowOptionPropagates(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerLogs(mock.Anything, "abc", mock.MatchedBy(func(opts container.LogsOptions) bool {
			return opts.Follow && opts.Tail == "100"
		})).
		Return(io.NopCloser(bytes.NewReader(nil)), nil).
		Once()

	stream, err := c.Logs(context.Background(), "abc", Follow(), Tail("100"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Drain to allow the goroutine to finish & close.
	for range stream {
	}
}

func TestLogs_SDKErrorReturnedSync(t *testing.T) {
	api := newMockdockerAPI(t)
	c := fromAPI(api)

	api.EXPECT().
		ContainerLogs(mock.Anything, "abc", mock.Anything).
		Return(nil, errors.New("no such container")).
		Once()

	if _, err := c.Logs(context.Background(), "abc"); err == nil {
		t.Fatal("expected error")
	}
}

// === helpers ===

// fakeSource is a Source that returns a canned tar (or error). Lets tests
// drive Build without a real vfs.Tree.
type fakeSource struct {
	tar io.ReadCloser
	err error
}

func (f *fakeSource) Tar(ctx context.Context) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tar, nil
}

// countingCloser wraps a Reader and counts Close calls.
type countingCloser struct {
	io.Reader
	closed int
}

func (c *countingCloser) Close() error { c.closed++; return nil }

// writeStdcopyFrame appends one stdcopy-multiplexed frame to buf.
func writeStdcopyFrame(buf *bytes.Buffer, stream byte, payload string) {
	hdr := [8]byte{stream, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	buf.Write(hdr[:])
	buf.WriteString(payload)
}
