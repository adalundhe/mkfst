package docker

import (
	"context"
	"strings"
	"testing"
)

func TestStreamJSONMessagesParsesStreamLine(t *testing.T) {
	input := `{"stream":"Step 1/3 : FROM scratch\n"}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	got := drain(out)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (stream + done): %+v", len(got), got)
	}
	if got[0].Kind != EventStream || got[0].Message != "Step 1/3 : FROM scratch\n" {
		t.Fatalf("stream event: %+v", got[0])
	}
	if got[1].Kind != EventDone {
		t.Fatalf("terminator: %+v", got[1])
	}
}

func TestStreamJSONMessagesParsesAuxImageID(t *testing.T) {
	input := `{"aux":{"ID":"sha256:abc123"}}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	got := drain(out)
	if got[0].Kind != EventAux {
		t.Fatalf("aux event: %+v", got[0])
	}
	if id := got[0].ImageID(); id != "sha256:abc123" {
		t.Fatalf("ImageID: %q", id)
	}
}

func TestStreamJSONMessagesEmitsErrorOnFailure(t *testing.T) {
	input := `{"errorDetail":{"message":"build failed"},"error":"build failed"}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	got := drain(out)
	if got[0].Kind != EventError || got[0].Message != "build failed" || got[0].Err == nil {
		t.Fatalf("error event: %+v", got[0])
	}
	// After EventError, channel should close (drain returns just the error).
	if len(got) != 1 {
		t.Fatalf("got %d events after error, want 1: %+v", len(got), got)
	}
}

func TestStreamJSONMessagesEmitsProgress(t *testing.T) {
	input := `{"id":"abc","status":"Downloading","progressDetail":{"current":50,"total":100},"progress":"[==>] 50B/100B"}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	got := drain(out)
	if got[0].Kind != EventProgress {
		t.Fatalf("first event: %+v", got[0])
	}
	if got[0].ID != "abc" {
		t.Fatalf("ID: %q", got[0].ID)
	}
	if got[0].Progress == nil || got[0].Progress.Current != 50 || got[0].Progress.Total != 100 {
		t.Fatalf("Progress: %+v", got[0].Progress)
	}
	if got[0].Progress.Detail != "[==>] 50B/100B" {
		t.Fatalf("Progress.Detail: %q", got[0].Progress.Detail)
	}
}

func TestStreamJSONMessagesEmitsStatus(t *testing.T) {
	input := `{"id":"layer1","status":"Pulling fs layer"}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	got := drain(out)
	if got[0].Kind != EventStatus {
		t.Fatalf("event kind: %+v", got[0])
	}
	if got[0].Message != "Pulling fs layer" {
		t.Fatalf("message: %q", got[0].Message)
	}
}

func TestDrainEventsReportsTerminalError(t *testing.T) {
	input := `{"stream":"step\n"}` + "\n" + `{"error":"oops","errorDetail":{"message":"oops"}}` + "\n"
	out := make(chan Event, 4)
	go streamJSONMessages(context.Background(), strings.NewReader(input), out)

	final, err := DrainEvents(out)
	if err == nil {
		t.Fatalf("expected error from DrainEvents")
	}
	if final.Kind != EventError {
		t.Fatalf("final event kind: %+v", final)
	}
}

// drain collects every event from out until close.
func drain(ch <-chan Event) []Event {
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}
