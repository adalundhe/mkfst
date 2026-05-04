// Package sqs is mkfst's AWS SQS wrapper. Exposes Send + Receive
// + Delete primitives plus a long-poll Subscribe that drives a
// channel of incoming messages with goroutine accounting.
package sqs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	mkawsutil "mkfst/providers/aws"
)

// sqsAPI is the narrow surface of the SQS SDK we call. The real
// *sqs.Client satisfies it implicitly; tests substitute a
// mockery-generated mock so they exercise the actual code paths
// without hitting AWS.
type sqsAPI interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Client wraps the AWS SQS client and an optional default queue URL.
type Client struct {
	cli         sqsAPI
	cfg         awssdk.Config
	defaultURL  string
}

// Opts configures NewClient.
type Opts struct {
	AWS mkawsutil.Opts

	// QueueURL, when non-empty, is the default queue every Send /
	// Receive / Delete uses if the per-call queueURL is empty.
	// Skip to require an explicit per-call URL.
	QueueURL string
}

// New builds a Client.
func New(ctx context.Context, opts Opts) (*Client, error) {
	cfg, err := mkawsutil.Resolve(ctx, opts.AWS)
	if err != nil {
		return nil, err
	}
	return &Client{
		cli:        sqs.NewFromConfig(cfg),
		cfg:        cfg,
		defaultURL: opts.QueueURL,
	}, nil
}

// FromConfig wraps an existing AWS config + optional default queue URL.
func FromConfig(cfg awssdk.Config, defaultQueueURL string) *Client {
	return &Client{cli: sqs.NewFromConfig(cfg), cfg: cfg, defaultURL: defaultQueueURL}
}

// fromAPI is the test-only constructor: builds a Client around a
// mock sqsAPI.
func fromAPI(api sqsAPI, defaultQueueURL string) *Client {
	return &Client{cli: api, defaultURL: defaultQueueURL}
}

// SDKConfig returns the resolved AWS config so callers can build
// their own SDK clients (or other AWS providers) sharing the same
// auth scope.
func (c *Client) SDKConfig() awssdk.Config { return c.cfg }

// Message is the receive-side view: every field the typical
// caller cares about, plus the receipt handle for ack.
type Message struct {
	ID            string            // SQS message id
	Body          []byte            // raw bytes
	Attributes    map[string]string // user message attributes (string-typed only)
	Receipt       string            // pass to Delete to ack
	ApproxReceive int               // ApproximateReceiveCount
	SentAt        time.Time         // SentTimestamp
}

// SendOpts configures Send.
type SendOpts struct {
	// QueueURL overrides the client's default. Required if the
	// client wasn't built with one.
	QueueURL string
	// Attributes is the user message-attribute map. String-typed
	// values only; for numeric / binary use SDK directly.
	Attributes map[string]string
	// DelaySeconds delays delivery (FIFO queues ignore).
	DelaySeconds int32
	// GroupID + DedupID are required for FIFO queues; ignored on
	// standard.
	GroupID string
	DedupID string
}

// Send publishes a message to the queue. Returns the assigned
// MessageId.
func (c *Client) Send(ctx context.Context, body []byte, opts SendOpts) (string, error) {
	url := opts.QueueURL
	if url == "" {
		url = c.defaultURL
	}
	if url == "" {
		return "", errors.New("sqs.Send: QueueURL required (set on client or per-call)")
	}
	in := &sqs.SendMessageInput{
		QueueUrl:    awssdk.String(url),
		MessageBody: awssdk.String(string(body)),
	}
	if opts.DelaySeconds > 0 {
		in.DelaySeconds = opts.DelaySeconds
	}
	if len(opts.Attributes) > 0 {
		in.MessageAttributes = map[string]types.MessageAttributeValue{}
		for k, v := range opts.Attributes {
			in.MessageAttributes[k] = types.MessageAttributeValue{
				DataType:    awssdk.String("String"),
				StringValue: awssdk.String(v),
			}
		}
	}
	if opts.GroupID != "" {
		in.MessageGroupId = awssdk.String(opts.GroupID)
	}
	if opts.DedupID != "" {
		in.MessageDeduplicationId = awssdk.String(opts.DedupID)
	}
	out, err := c.cli.SendMessage(ctx, in)
	if err != nil {
		return "", fmt.Errorf("sqs.Send: %w", err)
	}
	return awssdk.ToString(out.MessageId), nil
}

// ReceiveOpts configures Receive.
type ReceiveOpts struct {
	QueueURL          string
	MaxMessages       int32 // default 1, max 10
	WaitSeconds       int32 // default 20 (long-poll); 0 = short-poll
	VisibilityTimeout int32 // default 30s
	AttributeNames    []string
}

// Receive fetches up to MaxMessages messages from the queue.
// Returns an empty slice if none arrived within WaitSeconds.
func (c *Client) Receive(ctx context.Context, opts ReceiveOpts) ([]Message, error) {
	url := opts.QueueURL
	if url == "" {
		url = c.defaultURL
	}
	if url == "" {
		return nil, errors.New("sqs.Receive: QueueURL required")
	}
	if opts.MaxMessages <= 0 {
		opts.MaxMessages = 1
	}
	if opts.MaxMessages > 10 {
		opts.MaxMessages = 10
	}
	if opts.WaitSeconds == 0 {
		opts.WaitSeconds = 20
	}
	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = 30
	}

	in := &sqs.ReceiveMessageInput{
		QueueUrl:                  awssdk.String(url),
		MaxNumberOfMessages:       opts.MaxMessages,
		WaitTimeSeconds:           opts.WaitSeconds,
		VisibilityTimeout:         opts.VisibilityTimeout,
		MessageAttributeNames:     []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
			types.MessageSystemAttributeNameSentTimestamp,
		},
	}
	out, err := c.cli.ReceiveMessage(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("sqs.Receive: %w", err)
	}
	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		msg := Message{
			ID:      awssdk.ToString(m.MessageId),
			Body:    []byte(awssdk.ToString(m.Body)),
			Receipt: awssdk.ToString(m.ReceiptHandle),
		}
		if v, ok := m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]; ok {
			fmt.Sscanf(v, "%d", &msg.ApproxReceive)
		}
		if v, ok := m.Attributes[string(types.MessageSystemAttributeNameSentTimestamp)]; ok {
			var ms int64
			fmt.Sscanf(v, "%d", &ms)
			msg.SentAt = time.UnixMilli(ms)
		}
		if len(m.MessageAttributes) > 0 {
			msg.Attributes = make(map[string]string, len(m.MessageAttributes))
			for k, av := range m.MessageAttributes {
				if av.StringValue != nil {
					msg.Attributes[k] = *av.StringValue
				}
			}
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// Delete acks a message via its receipt handle. Idempotent: SQS
// won't error on a double-delete (the second call no-ops once the
// receipt expires).
func (c *Client) Delete(ctx context.Context, queueURL, receipt string) error {
	url := queueURL
	if url == "" {
		url = c.defaultURL
	}
	if url == "" {
		return errors.New("sqs.Delete: QueueURL required")
	}
	if receipt == "" {
		return errors.New("sqs.Delete: receipt required")
	}
	_, err := c.cli.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      awssdk.String(url),
		ReceiptHandle: awssdk.String(receipt),
	})
	if err != nil {
		return fmt.Errorf("sqs.Delete: %w", err)
	}
	return nil
}

// === Subscribe (long-poll consumer loop) ===

// SubscribeOpts configures Subscribe.
type SubscribeOpts struct {
	QueueURL          string
	MaxMessages       int32 // default 10 (most efficient)
	WaitSeconds       int32 // default 20 (long-poll)
	VisibilityTimeout int32 // default 30s
	// Concurrency caps in-flight handler invocations. 0 defaults
	// to the MaxMessages value (one goroutine per claimed message).
	Concurrency int
	// AutoAck, when true, deletes a message after Handler
	// returns nil. False leaves ack to the handler (call
	// client.Delete with msg.Receipt explicitly).
	AutoAck bool
	// Handler is invoked for each received message. Required.
	Handler func(ctx context.Context, msg Message) error
	// OnError is invoked from the receive loop for transient
	// errors. nil = silent.
	OnError func(err error)
}

// Subscribe starts a long-poll consumer loop running in a tracked
// goroutine. It exits cleanly when ctx cancels; the returned
// channel closes once all in-flight handlers have finished. Errors
// from Receive surface via OnError; per-message Handler errors
// surface the same way.
func (c *Client) Subscribe(ctx context.Context, opts SubscribeOpts) (<-chan struct{}, error) {
	if opts.Handler == nil {
		return nil, errors.New("sqs.Subscribe: Handler required")
	}
	url := opts.QueueURL
	if url == "" {
		url = c.defaultURL
	}
	if url == "" {
		return nil, errors.New("sqs.Subscribe: QueueURL required")
	}
	if opts.MaxMessages <= 0 {
		opts.MaxMessages = 10
	}
	if opts.WaitSeconds == 0 {
		opts.WaitSeconds = 20
	}
	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = 30
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = int(opts.MaxMessages)
	}
	sem := make(chan struct{}, opts.Concurrency)
	done := make(chan struct{})
	var inflight sync.WaitGroup
	go func() {
		defer close(done)
		for {
			if err := ctx.Err(); err != nil {
				inflight.Wait()
				return
			}
			msgs, err := c.Receive(ctx, ReceiveOpts{
				QueueURL:          url,
				MaxMessages:       opts.MaxMessages,
				WaitSeconds:       opts.WaitSeconds,
				VisibilityTimeout: opts.VisibilityTimeout,
			})
			if err != nil {
				if opts.OnError != nil && !errors.Is(err, context.Canceled) {
					opts.OnError(err)
				}
				select {
				case <-ctx.Done():
					inflight.Wait()
					return
				case <-time.After(time.Second):
				}
				continue
			}
			for _, m := range msgs {
				m := m
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					inflight.Wait()
					return
				}
				inflight.Add(1)
				go func() {
					defer func() { <-sem; inflight.Done() }()
					hErr := opts.Handler(ctx, m)
					if hErr != nil {
						if opts.OnError != nil {
							opts.OnError(fmt.Errorf("handler msg=%s: %w", m.ID, hErr))
						}
						return // don't ack on failure → message becomes visible again after VisibilityTimeout
					}
					if opts.AutoAck {
						if delErr := c.Delete(ctx, url, m.Receipt); delErr != nil && opts.OnError != nil {
							opts.OnError(fmt.Errorf("auto-ack msg=%s: %w", m.ID, delErr))
						}
					}
				}()
			}
		}
	}()
	return done, nil
}
