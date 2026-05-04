package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// LogStream identifies which standard stream a LogLine came from.
type LogStream string

const (
	LogStreamStdout LogStream = "stdout"
	LogStreamStderr LogStream = "stderr"
)

// LogLine is one line of container output. The terminal newline is
// stripped from Message so callers don't have to.
type LogLine struct {
	Stream    LogStream
	Timestamp time.Time
	Message   string
	Err       error
}

// LogsOption mutates the assembled logs request.
type LogsOption func(*logsState)

type logsState struct {
	opts container.LogsOptions
}

// Logs streams a container's stdout/stderr (and historical lines) as a
// channel of LogLine. The channel closes when the daemon ends the stream
// (container stops + Follow not set, or external close); a terminal error
// appears as a LogLine with Err set before the close.
//
// stdcopy demuxes docker's multiplexed output format so each LogLine is
// tagged with its source stream. Use Stdout()/Stderr() to filter at the
// daemon (saves bandwidth) or filter on Stream client-side.
func (c *Client) Logs(ctx context.Context, containerID string, opts ...LogsOption) (<-chan LogLine, error) {
	state := &logsState{
		opts: container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		},
	}
	for _, opt := range opts {
		opt(state)
	}

	rc, err := c.api.ContainerLogs(ctx, containerID, state.opts)
	if err != nil {
		return nil, fmt.Errorf("docker.Logs: %w", err)
	}

	out := make(chan LogLine, 32)
	go func() {
		defer close(out)
		defer rc.Close()
		streamLogs(ctx, rc, out, state.opts.Timestamps)
	}()
	return out, nil
}

// streamLogs demuxes the daemon's stream into per-line LogLine values. The
// daemon emits multiplexed frames: stdcopy.StdCopy splits them into
// stdout/stderr writers; we wrap each writer with a bufio.Scanner so each
// emitted "line" hits the channel as one event.
func streamLogs(ctx context.Context, r io.Reader, out chan<- LogLine, timestamps bool) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// Goroutines for each stream so we can scan them concurrently.
	doneCh := make(chan struct{}, 2)
	go scanStream(ctx, stdoutR, LogStreamStdout, out, timestamps, doneCh)
	go scanStream(ctx, stderrR, LogStreamStderr, out, timestamps, doneCh)

	_, copyErr := stdcopy.StdCopy(stdoutW, stderrW, r)

	// Close the pipes so the scanners hit EOF.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	// Wait for both scanners.
	<-doneCh
	<-doneCh

	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		select {
		case out <- LogLine{Err: fmt.Errorf("logs stream: %w", copyErr)}:
		case <-ctx.Done():
		}
	}
}

// scanStream reads lines from r and emits them as LogLines tagged with
// stream. timestamps controls whether the leading RFC3339Nano timestamp is
// parsed into LogLine.Timestamp (and stripped from Message). Sends to
// doneCh on exit so the caller can wait.
func scanStream(ctx context.Context, r io.Reader, stream LogStream, out chan<- LogLine, timestamps bool, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	sc := bufio.NewScanner(r)
	// Bump the default 64 KiB buffer to handle log lines from chatty
	// programs (panic stacktraces, JSON-formatted requests, etc.).
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return
		}
		line := LogLine{Stream: stream}
		raw := sc.Bytes()
		if timestamps && len(raw) > 0 {
			ts, msg := splitTimestampLine(raw)
			line.Timestamp = ts
			line.Message = msg
		} else {
			line.Message = string(raw)
		}
		select {
		case out <- line:
		case <-ctx.Done():
			return
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		select {
		case out <- LogLine{Stream: stream, Err: err}:
		case <-ctx.Done():
		}
	}
}

// splitTimestampLine separates the leading RFC3339Nano timestamp from the
// rest of a log line. Docker's format is "2006-01-02T15:04:05.999999999Z msg".
// Falls back gracefully if parsing fails (returns zero time + the raw line).
func splitTimestampLine(raw []byte) (time.Time, string) {
	idx := bytes.IndexByte(raw, ' ')
	if idx <= 0 {
		return time.Time{}, string(raw)
	}
	tsStr := string(raw[:idx])
	rest := strings.TrimLeft(string(raw[idx+1:]), " ")
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return time.Time{}, string(raw)
	}
	return ts, rest
}

// === Logs options ===

// Follow streams new output as it's emitted (like docker logs -f).
// Without this, only existing buffered output is returned and the stream
// closes immediately.
func Follow() LogsOption {
	return func(s *logsState) { s.opts.Follow = true }
}

// Since limits the stream to messages emitted at or after t.
func Since(t time.Time) LogsOption {
	return func(s *logsState) { s.opts.Since = t.Format(time.RFC3339Nano) }
}

// Until limits the stream to messages emitted at or before t.
func Until(t time.Time) LogsOption {
	return func(s *logsState) { s.opts.Until = t.Format(time.RFC3339Nano) }
}

// Tail returns only the last n lines of buffered output before
// (optionally) following. "all" returns the full buffer.
func Tail(n string) LogsOption {
	return func(s *logsState) { s.opts.Tail = n }
}

// Timestamps prefixes each line with an RFC3339Nano timestamp at the
// daemon, which we then parse into LogLine.Timestamp.
func Timestamps() LogsOption {
	return func(s *logsState) { s.opts.Timestamps = true }
}

// Stdout-only filters out stderr at the daemon. Saves bandwidth on chatty
// stderr output you don't care about.
func Stdout() LogsOption {
	return func(s *logsState) {
		s.opts.ShowStdout = true
		s.opts.ShowStderr = false
	}
}

// Stderr-only filters out stdout at the daemon.
func Stderr() LogsOption {
	return func(s *logsState) {
		s.opts.ShowStdout = false
		s.opts.ShowStderr = true
	}
}
