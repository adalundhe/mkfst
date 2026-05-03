package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/pkg/jsonmessage"
)

// EventKind discriminates the lifecycle stages emitted by long-running
// operations (Build, Pull). One operation produces zero-or-more
// Stream/Status/Progress/Aux events followed by exactly one terminator
// (Done or Error).
type EventKind string

const (
	// EventStream is a chunk of human-readable output (build step output,
	// daemon messages). Carried in Event.Message.
	EventStream EventKind = "stream"
	// EventStatus is a short-lived status message for a layer or step
	// (e.g. "Pulling fs layer", "Verifying checksum"). Often paired with
	// an ID identifying the layer.
	EventStatus EventKind = "status"
	// EventProgress is a progress update on a layer download/upload.
	// Detail in Event.Progress (current/total bytes); ID identifies the
	// layer the progress applies to.
	EventProgress EventKind = "progress"
	// EventAux carries structured side-band data that doesn't fit the
	// other shapes — most importantly, the final image ID after a
	// successful build. Raw JSON is preserved in Event.Aux.
	EventAux EventKind = "aux"
	// EventError signals a terminal failure. The channel closes
	// immediately after this event; callers should not expect more.
	EventError EventKind = "error"
	// EventDone signals successful completion. The channel closes
	// immediately after.
	EventDone EventKind = "done"
)

// Event is one emission from a streaming docker operation. Per-Kind fields
// are documented inline; fields irrelevant to a given Kind are zero-valued.
type Event struct {
	Kind EventKind

	// ID identifies what the event pertains to (a layer hash, a build step
	// number, etc.). Empty for general-purpose stream/status events.
	ID string

	// Message is the human-readable text — the body of stream events, the
	// status text of status events, the error text of error events.
	Message string

	// Progress carries byte-counts for EventProgress events. nil otherwise.
	Progress *Progress

	// Aux holds the raw JSON payload for EventAux events. Common shapes:
	//   build:  {"ID":"sha256:..."}        — final image ID
	//   pull:   {"ID":"sha256:..."}        — manifest digest
	// Decoded structures are exposed via convenience helpers below
	// (e.g. Event.ImageID).
	Aux json.RawMessage

	// Err is the underlying error for EventError events. nil otherwise.
	Err error
}

// Progress is the byte-counter shape for EventProgress events.
type Progress struct {
	Current int64  // bytes transferred so far
	Total   int64  // total bytes (0 if unknown)
	Detail  string // free-form detail line ("[========> ] 12.34MB/45.67MB")
}

// ImageID returns the sha256-prefixed image identifier from an EventAux
// event, if present. Empty string when the aux payload is missing or has a
// different shape (e.g. some pull aux events carry only a manifest digest).
func (e Event) ImageID() string {
	if e.Kind != EventAux || len(e.Aux) == 0 {
		return ""
	}
	var aux struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(e.Aux, &aux); err != nil {
		return ""
	}
	return aux.ID
}

// streamJSONMessages reads docker's jsonmessage stream from r and writes
// translated Events to out. Closes out when r returns EOF or ctx is
// cancelled. A non-EOF read error or a stream-embedded error message
// produces a final EventError before close.
//
// docker's jsonmessage format wraps several distinct event shapes into one
// newline-delimited JSON stream; each line is a jsonmessage.JSONMessage
// with optional embedded ID, Status, Progress, Stream, Aux, ErrorMessage.
// We dispatch based on which fields are populated to produce the right
// EventKind.
func streamJSONMessages(ctx context.Context, r io.Reader, out chan<- Event) {
	defer close(out)

	dec := json.NewDecoder(r)
	for {
		if err := ctx.Err(); err != nil {
			send(ctx, out, Event{Kind: EventError, Err: err})
			return
		}
		var msg jsonmessage.JSONMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				send(ctx, out, Event{Kind: EventDone})
				return
			}
			send(ctx, out, Event{Kind: EventError, Err: fmt.Errorf("decode jsonmessage: %w", err)})
			return
		}
		if msg.Error != nil {
			send(ctx, out, Event{
				Kind:    EventError,
				ID:      msg.ID,
				Message: msg.Error.Message,
				Err:     fmt.Errorf("%s", msg.Error.Message),
			})
			return
		}
		if msg.ErrorMessage != "" {
			send(ctx, out, Event{
				Kind:    EventError,
				ID:      msg.ID,
				Message: msg.ErrorMessage,
				Err:     fmt.Errorf("%s", msg.ErrorMessage),
			})
			return
		}

		ev := translateMessage(msg)
		if ev.Kind == "" {
			continue // empty/keepalive frame
		}
		send(ctx, out, ev)
	}
}

// translateMessage maps a single decoded jsonmessage into the right Event
// kind. Returns a zero Event (Kind=="") for empty/uninteresting frames so
// the caller can skip them.
func translateMessage(msg jsonmessage.JSONMessage) Event {
	switch {
	case msg.Aux != nil:
		return Event{Kind: EventAux, ID: msg.ID, Aux: *msg.Aux}
	case msg.Stream != "":
		return Event{Kind: EventStream, ID: msg.ID, Message: msg.Stream}
	case msg.Progress != nil && (msg.Progress.Current != 0 || msg.Progress.Total != 0):
		return Event{
			Kind: EventProgress,
			ID:   msg.ID,
			Progress: &Progress{
				Current: msg.Progress.Current,
				Total:   msg.Progress.Total,
				Detail:  msg.ProgressMessage,
			},
			Message: msg.Status,
		}
	case msg.Status != "":
		return Event{Kind: EventStatus, ID: msg.ID, Message: msg.Status}
	}
	return Event{}
}

// send delivers ev to out unless ctx fires first. Used so a slow consumer
// doesn't pin the producer goroutine forever — the producer exits when ctx
// is cancelled, leaving the channel half-open from the consumer's POV until
// they too notice the cancel.
func send(ctx context.Context, out chan<- Event, ev Event) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

// drainEvents is a convenience for callers who want a synchronous "build
// completes or error" semantics — collects every event, returns the final
// EventDone or EventError. Mostly useful in examples and tests; production
// code typically wants to react to the stream as it flows.
func DrainEvents(events <-chan Event) (final Event, err error) {
	for ev := range events {
		final = ev
		if ev.Kind == EventError {
			err = ev.Err
		}
	}
	return final, err
}
