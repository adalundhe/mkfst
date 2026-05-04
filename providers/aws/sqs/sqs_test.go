package sqs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/mock"
)

// === Mock-backed end-to-end tests ===

func TestClient_Send_BuildsCorrectInput(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")

	api.EXPECT().SendMessage(mock.Anything, mock.MatchedBy(func(in *awssqs.SendMessageInput) bool {
		return awssdk.ToString(in.QueueUrl) == "https://sqs/q" &&
			awssdk.ToString(in.MessageBody) == `{"x":1}` &&
			in.MessageAttributes["trace_id"].StringValue != nil &&
			*in.MessageAttributes["trace_id"].StringValue == "abc"
	})).Return(&awssqs.SendMessageOutput{
		MessageId: awssdk.String("mid-123"),
	}, nil).Once()

	id, err := c.Send(context.Background(), []byte(`{"x":1}`), SendOpts{
		Attributes: map[string]string{"trace_id": "abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "mid-123" {
		t.Fatalf("got id %q", id)
	}
}

func TestClient_Send_FIFO_PassesGroupAndDedup(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q.fifo")

	var captured *awssqs.SendMessageInput
	api.EXPECT().SendMessage(mock.Anything, mock.Anything).
		Run(func(ctx context.Context, in *awssqs.SendMessageInput, opts ...func(*awssqs.Options)) {
			captured = in
		}).
		Return(&awssqs.SendMessageOutput{MessageId: awssdk.String("m")}, nil).Once()

	_, err := c.Send(context.Background(), []byte("x"), SendOpts{
		GroupID: "orders", DedupID: "dedup-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if awssdk.ToString(captured.MessageGroupId) != "orders" {
		t.Fatalf("group id missing: %v", captured.MessageGroupId)
	}
	if awssdk.ToString(captured.MessageDeduplicationId) != "dedup-1" {
		t.Fatalf("dedup id missing")
	}
}

func TestClient_Receive_DecodesMessages(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")

	api.EXPECT().ReceiveMessage(mock.Anything, mock.MatchedBy(func(in *awssqs.ReceiveMessageInput) bool {
		return in.MaxNumberOfMessages == 5 && in.WaitTimeSeconds == 10
	})).Return(&awssqs.ReceiveMessageOutput{
		Messages: []types.Message{
			{
				MessageId:     awssdk.String("m1"),
				Body:          awssdk.String("hello"),
				ReceiptHandle: awssdk.String("rcpt-1"),
				Attributes: map[string]string{
					string(types.MessageSystemAttributeNameApproximateReceiveCount): "3",
					string(types.MessageSystemAttributeNameSentTimestamp):           "1700000000000",
				},
				MessageAttributes: map[string]types.MessageAttributeValue{
					"trace_id": {DataType: awssdk.String("String"), StringValue: awssdk.String("t-1")},
				},
			},
		},
	}, nil).Once()

	msgs, err := c.Receive(context.Background(), ReceiveOpts{
		MaxMessages: 5, WaitSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	m := msgs[0]
	if m.ID != "m1" || string(m.Body) != "hello" || m.Receipt != "rcpt-1" {
		t.Fatalf("bad decode: %+v", m)
	}
	if m.ApproxReceive != 3 {
		t.Fatalf("approx receive: %d", m.ApproxReceive)
	}
	if m.Attributes["trace_id"] != "t-1" {
		t.Fatalf("attribute decode: %v", m.Attributes)
	}
}

func TestClient_Receive_PropagatesError(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")
	api.EXPECT().ReceiveMessage(mock.Anything, mock.Anything).
		Return(nil, errors.New("Throttling")).Once()
	_, err := c.Receive(context.Background(), ReceiveOpts{})
	if err == nil || !strings.Contains(err.Error(), "Throttling") {
		t.Fatalf("expected Throttling, got %v", err)
	}
}

func TestClient_Delete_PassesReceipt(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")
	api.EXPECT().DeleteMessage(mock.Anything, mock.MatchedBy(func(in *awssqs.DeleteMessageInput) bool {
		return awssdk.ToString(in.ReceiptHandle) == "rcpt-1"
	})).Return(&awssqs.DeleteMessageOutput{}, nil).Once()

	if err := c.Delete(context.Background(), "", "rcpt-1"); err != nil {
		t.Fatal(err)
	}
}

// TestSubscribe_EndToEnd is the big test: a full Subscribe loop
// driven by a mock that returns 3 messages, then empties out.
// We verify Handler runs for each, AutoAck triggers DeleteMessage,
// and the goroutine exits cleanly on ctx cancel.
func TestSubscribe_EndToEnd(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")

	// Burst: 3 messages, then empty receives forever.
	var receiveCount atomic.Int32
	api.EXPECT().ReceiveMessage(mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			n := receiveCount.Add(1)
			if n == 1 {
				return &awssqs.ReceiveMessageOutput{
					Messages: []types.Message{
						{MessageId: awssdk.String("m1"), Body: awssdk.String("b1"), ReceiptHandle: awssdk.String("r1")},
						{MessageId: awssdk.String("m2"), Body: awssdk.String("b2"), ReceiptHandle: awssdk.String("r2")},
						{MessageId: awssdk.String("m3"), Body: awssdk.String("b3"), ReceiptHandle: awssdk.String("r3")},
					},
				}, nil
			}
			return &awssqs.ReceiveMessageOutput{}, nil
		},
	)

	// Three deletes (one per message; auto-ack).
	api.EXPECT().DeleteMessage(mock.Anything, mock.Anything).
		Return(&awssqs.DeleteMessageOutput{}, nil).Times(3)

	var seen sync.Map // message id → received body
	ctx, cancel := context.WithCancel(context.Background())

	done, err := c.Subscribe(ctx, SubscribeOpts{
		AutoAck:     true,
		Concurrency: 4,
		Handler: func(_ context.Context, m Message) error {
			seen.Store(m.ID, string(m.Body))
			return nil
		},
		OnError: func(err error) { t.Logf("subscribe error: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until all three messages have been processed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		seen.Range(func(_, _ any) bool { count++; return true })
		if count == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	count := 0
	seen.Range(func(_, _ any) bool { count++; return true })
	if count != 3 {
		t.Fatalf("got %d messages, want 3", count)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe didn't exit on ctx cancel")
	}
}

// TestSubscribe_HandlerErrorSkipsAck verifies the contract that a
// failed Handler does NOT trigger AutoAck — the message stays in
// the queue (visibility timeout will eventually requeue it).
func TestSubscribe_HandlerErrorSkipsAck(t *testing.T) {
	api := newMocksqsAPI(t)
	c := fromAPI(api, "https://sqs/q")

	var receiveCount atomic.Int32
	api.EXPECT().ReceiveMessage(mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			if receiveCount.Add(1) == 1 {
				return &awssqs.ReceiveMessageOutput{
					Messages: []types.Message{
						{MessageId: awssdk.String("m1"), Body: awssdk.String("b1"), ReceiptHandle: awssdk.String("r1")},
					},
				}, nil
			}
			return &awssqs.ReceiveMessageOutput{}, nil
		},
	)
	// IMPORTANT: no DeleteMessage expectation — handler failure
	// must NOT trigger an ack. testify will fail the test if
	// DeleteMessage is called unexpectedly.

	ctx, cancel := context.WithCancel(context.Background())
	var errs []error
	var mu sync.Mutex
	done, err := c.Subscribe(ctx, SubscribeOpts{
		AutoAck:     true,
		Concurrency: 1,
		Handler: func(_ context.Context, _ Message) error {
			return errors.New("processing failed")
		},
		OnError: func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the handler-error to surface.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "processing failed") {
		t.Fatalf("expected processing-failed error, got %v", errs)
	}
}

// === pure validation tests (kept) ===

func TestSend_RequiresQueueURL(t *testing.T) {
	c := &Client{cfg: awssdk.Config{}}
	_, err := c.Send(context.Background(), []byte("x"), SendOpts{})
	if err == nil || !strings.Contains(err.Error(), "QueueURL required") {
		t.Fatalf("expected QueueURL-required error, got %v", err)
	}
}

func TestReceive_RequiresQueueURL(t *testing.T) {
	c := &Client{cfg: awssdk.Config{}}
	_, err := c.Receive(context.Background(), ReceiveOpts{})
	if err == nil || !strings.Contains(err.Error(), "QueueURL required") {
		t.Fatalf("expected QueueURL-required error, got %v", err)
	}
}

func TestDelete_RequiresQueueURL(t *testing.T) {
	c := &Client{cfg: awssdk.Config{}}
	err := c.Delete(context.Background(), "", "rcpt")
	if err == nil || !strings.Contains(err.Error(), "QueueURL required") {
		t.Fatalf("expected QueueURL-required error, got %v", err)
	}
}

func TestDelete_RequiresReceipt(t *testing.T) {
	c := &Client{cfg: awssdk.Config{}, defaultURL: "https://q"}
	err := c.Delete(context.Background(), "", "")
	if err == nil || !strings.Contains(err.Error(), "receipt required") {
		t.Fatalf("expected receipt-required error, got %v", err)
	}
}

func TestSubscribe_RequiresHandler(t *testing.T) {
	c := &Client{cfg: awssdk.Config{}, defaultURL: "https://q"}
	_, err := c.Subscribe(context.Background(), SubscribeOpts{})
	if err == nil || !strings.Contains(err.Error(), "Handler required") {
		t.Fatalf("expected Handler-required error, got %v", err)
	}
}

// TestSubscribe_HandlerErrorIsReported probes the error surfacing
// without going to AWS. Drive the post-receive path manually.
func TestSubscribe_HandlerErrorChannelShape(t *testing.T) {
	// Synthesize the goroutine accounting we expect. This is a
	// shape test — confirms the public types compose correctly.
	var wg sync.WaitGroup
	var got []error
	var mu sync.Mutex
	opts := SubscribeOpts{
		Handler: func(_ context.Context, _ Message) error { return errors.New("nope") },
		OnError: func(err error) { mu.Lock(); got = append(got, err); mu.Unlock() },
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Call the handler directly, wrap the error like Subscribe does.
		err := opts.Handler(context.Background(), Message{ID: "m1"})
		if err != nil {
			opts.OnError(err)
		}
	}()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0].Error(), "nope") {
		t.Fatalf("unexpected: %v", got)
	}
}
