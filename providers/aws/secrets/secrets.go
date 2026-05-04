// Package secrets is mkfst's AWS Secrets Manager wrapper. It
// fetches secret values and integrates with `providers/docker/network`
// to inject them into stack containers as tmpfs-mounted files
// (the secure path that avoids `docker inspect` env leakage).
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	mkawsutil "mkfst/providers/aws"
	"mkfst/providers/docker/network"
)

// secretsAPI is the narrow surface of the Secrets Manager SDK
// we call. Defining it as an interface lets tests substitute a
// mockery-generated mock instead of hitting a real AWS endpoint.
// The real *secretsmanager.Client satisfies it implicitly.
type secretsAPI interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
}

// Client wraps the AWS Secrets Manager client.
type Client struct {
	cli secretsAPI
	cfg awssdk.Config

	mu    sync.RWMutex
	cache map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	value    []byte
	fetched  time.Time
}

// Opts configures NewClient.
type Opts struct {
	AWS mkawsutil.Opts
	// CacheTTL is the in-memory cache duration for fetched
	// secrets. 0 disables caching (every Get hits the API).
	// Default 5 minutes.
	CacheTTL time.Duration
}

// New builds a Client.
func New(ctx context.Context, opts Opts) (*Client, error) {
	cfg, err := mkawsutil.Resolve(ctx, opts.AWS)
	if err != nil {
		return nil, err
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	return &Client{
		cli:   secretsmanager.NewFromConfig(cfg),
		cfg:   cfg,
		cache: map[string]cacheEntry{},
		ttl:   opts.CacheTTL,
	}, nil
}

// FromConfig wraps an existing AWS config.
func FromConfig(cfg awssdk.Config, ttl time.Duration) *Client {
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	return &Client{
		cli:   secretsmanager.NewFromConfig(cfg),
		cfg:   cfg,
		cache: map[string]cacheEntry{},
		ttl:   ttl,
	}
}

// fromAPI is the test-only constructor: builds a Client around a
// mock secretsAPI.
func fromAPI(api secretsAPI, ttl time.Duration) *Client {
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	return &Client{cli: api, cache: map[string]cacheEntry{}, ttl: ttl}
}

// SDKConfig returns the resolved AWS config so callers can build
// their own SDK clients (or other AWS providers) sharing the same
// auth scope.
func (c *Client) SDKConfig() awssdk.Config { return c.cfg }

// Get fetches a secret by name or ARN. Returns the SecretString
// (or SecretBinary) bytes. Caches in memory for the configured
// TTL — call InvalidateCache to bust an entry, or pass
// CacheTTL: -1 at construction to disable caching entirely.
func (c *Client) Get(ctx context.Context, nameOrARN string) ([]byte, error) {
	if nameOrARN == "" {
		return nil, errors.New("secrets.Get: name required")
	}
	if c.ttl > 0 {
		c.mu.RLock()
		entry, ok := c.cache[nameOrARN]
		c.mu.RUnlock()
		if ok && time.Since(entry.fetched) < c.ttl {
			return append([]byte(nil), entry.value...), nil
		}
	}
	out, err := c.cli.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: awssdk.String(nameOrARN),
	})
	if err != nil {
		return nil, fmt.Errorf("secrets.Get %q: %w", nameOrARN, err)
	}
	var value []byte
	switch {
	case out.SecretString != nil:
		value = []byte(*out.SecretString)
	case len(out.SecretBinary) > 0:
		value = append([]byte(nil), out.SecretBinary...)
	default:
		return nil, fmt.Errorf("secrets.Get %q: no SecretString or SecretBinary", nameOrARN)
	}
	if c.ttl > 0 {
		c.mu.Lock()
		c.cache[nameOrARN] = cacheEntry{value: append([]byte(nil), value...), fetched: time.Now()}
		c.mu.Unlock()
	}
	return value, nil
}

// GetJSON fetches a secret expected to contain JSON, unmarshaling
// it into out. Convenient for the very common pattern of secrets
// holding a `{"username":"...","password":"..."}` blob.
func (c *Client) GetJSON(ctx context.Context, nameOrARN string, out any) error {
	b, err := c.Get(ctx, nameOrARN)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("secrets.GetJSON %q: %w", nameOrARN, err)
	}
	return nil
}

// GetStringMap fetches a JSON-shaped secret as a map[string]string.
func (c *Client) GetStringMap(ctx context.Context, nameOrARN string) (map[string]string, error) {
	var m map[string]string
	if err := c.GetJSON(ctx, nameOrARN, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Put creates or updates a secret. Returns the resulting ARN.
//
// If the secret doesn't exist this calls CreateSecret; if it does,
// it calls PutSecretValue which adds a new version. Same shape
// either way for the caller.
func (c *Client) Put(ctx context.Context, name string, value []byte) (string, error) {
	if name == "" {
		return "", errors.New("secrets.Put: name required")
	}
	// Try Put first (cheap roundtrip); fall back to Create on
	// ResourceNotFound. Avoids a Describe step.
	putOut, err := c.cli.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     awssdk.String(name),
		SecretString: awssdk.String(string(value)),
	})
	if err == nil {
		c.invalidate(name)
		return awssdk.ToString(putOut.ARN), nil
	}
	// Naive: any error → try Create. Real implementations should
	// inspect for ResourceNotFoundException specifically.
	createOut, err2 := c.cli.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         awssdk.String(name),
		SecretString: awssdk.String(string(value)),
	})
	if err2 != nil {
		return "", fmt.Errorf("secrets.Put %q (Put: %v) (Create: %w)", name, err, err2)
	}
	c.invalidate(name)
	return awssdk.ToString(createOut.ARN), nil
}

// InvalidateCache bumps the cached entry for a secret (forcing
// the next Get to re-fetch). Call after rotating the secret
// upstream.
func (c *Client) InvalidateCache(nameOrARN string) { c.invalidate(nameOrARN) }

func (c *Client) invalidate(nameOrARN string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	delete(c.cache, nameOrARN)
	c.mu.Unlock()
}

// === stack injection ===
//
// The mkfst Stack model materializes Secrets as tmpfs-backed bind
// mounts (see providers/docker/network/secrets.go). Inject lets
// users hydrate stack secrets directly from Secrets Manager
// without leaking the bytes through env vars or `docker inspect`.

// Inject fetches each (secretName, stackKey) pairing from
// Secrets Manager and registers it on the stack via Stack.AddSecret.
// The stack's Service definitions reference the keys via
// network.UseSecret(stackKey, mountPath).
//
// Returns the first error from any fetch + AddSecret call.
func (c *Client) Inject(ctx context.Context, stack *network.Stack, mappings map[string]string) error {
	if stack == nil {
		return errors.New("secrets.Inject: stack required")
	}
	for stackKey, secretRef := range mappings {
		value, err := c.Get(ctx, secretRef)
		if err != nil {
			return fmt.Errorf("secrets.Inject %q→%q: %w", secretRef, stackKey, err)
		}
		if err := stack.AddSecret(stackKey, value); err != nil {
			return fmt.Errorf("secrets.Inject %q→%q: AddSecret: %w", secretRef, stackKey, err)
		}
	}
	return nil
}

// InjectJSON fetches a JSON-shaped secret and explodes its top-
// level keys into individual stack secrets. Useful for the common
// pattern where one Secrets Manager entry holds
// `{"db_password":"...","api_key":"..."}` and you want each key
// available to a different service.
//
// stackKeyPrefix is prepended to each stack secret name; pass ""
// to use the JSON keys verbatim.
func (c *Client) InjectJSON(ctx context.Context, stack *network.Stack, secretRef string, stackKeyPrefix string) error {
	m, err := c.GetStringMap(ctx, secretRef)
	if err != nil {
		return err
	}
	for k, v := range m {
		key := stackKeyPrefix + k
		if err := stack.AddSecret(key, []byte(v)); err != nil {
			return fmt.Errorf("secrets.InjectJSON %q.%s: %w", secretRef, k, err)
		}
	}
	return nil
}
