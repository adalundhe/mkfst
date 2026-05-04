package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/stretchr/testify/mock"
)

// === Mock-backed end-to-end tests ===

func TestClient_Get_HitFromAPI(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.MatchedBy(func(in *secretsmanager.GetSecretValueInput) bool {
		return awssdk.ToString(in.SecretId) == "prod/db/password"
	})).Return(&secretsmanager.GetSecretValueOutput{
		SecretString: awssdk.String("hunter2"),
	}, nil).Once()

	b, err := c.Get(context.Background(), "prod/db/password")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hunter2" {
		t.Fatalf("got %q", string(b))
	}
}

func TestClient_Get_BinarySecret(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.GetSecretValueOutput{
			SecretBinary: []byte{0x01, 0x02, 0x03},
		}, nil).Once()
	b, err := c.Get(context.Background(), "binary-secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 3 || b[0] != 1 || b[2] != 3 {
		t.Fatalf("binary mismatch: %v", b)
	}
}

func TestClient_Get_NoSecretValue_Error(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.GetSecretValueOutput{}, nil).Once()
	_, err := c.Get(context.Background(), "empty")
	if err == nil || !strings.Contains(err.Error(), "no SecretString or SecretBinary") {
		t.Fatalf("expected empty-secret error, got %v", err)
	}
}

func TestClient_Get_PropagatesAPIError(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.Anything).
		Return(nil, errors.New("DenyAccess")).Once()
	_, err := c.Get(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "DenyAccess") {
		t.Fatalf("expected DenyAccess, got %v", err)
	}
}

func TestClient_Get_HitsCacheOnSecondCall(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.GetSecretValueOutput{SecretString: awssdk.String("v")}, nil).Once()

	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_GetJSON_UnmarshalsStruct(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().GetSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.GetSecretValueOutput{
			SecretString: awssdk.String(`{"username":"admin","password":"hunter2"}`),
		}, nil).Once()
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.GetJSON(context.Background(), "creds", &creds); err != nil {
		t.Fatal(err)
	}
	if creds.Username != "admin" || creds.Password != "hunter2" {
		t.Fatalf("unmarshal mismatch: %+v", creds)
	}
}

func TestClient_Put_TriesPutThenCreate(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().PutSecretValue(mock.Anything, mock.Anything).
		Return(nil, errors.New("ResourceNotFoundException")).Once()
	api.EXPECT().CreateSecret(mock.Anything, mock.Anything).
		Return(&secretsmanager.CreateSecretOutput{
			ARN: awssdk.String("arn:aws:secretsmanager:us-east-1:123:secret:new-secret-X"),
		}, nil).Once()

	arn, err := c.Put(context.Background(), "new-secret", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(arn, "new-secret") {
		t.Fatalf("arn: %q", arn)
	}
}

func TestClient_Put_UsesPutForExistingSecret(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)
	api.EXPECT().PutSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.PutSecretValueOutput{
			ARN: awssdk.String("arn:aws:...:existing"),
		}, nil).Once()
	arn, err := c.Put(context.Background(), "existing", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(arn, "existing") {
		t.Fatalf("arn: %q", arn)
	}
}

func TestClient_Put_InvalidatesCache(t *testing.T) {
	api := newMocksecretsAPI(t)
	c := fromAPI(api, time.Minute)

	c.mu.Lock()
	c.cache["k"] = cacheEntry{value: []byte("old"), fetched: time.Now()}
	c.mu.Unlock()

	api.EXPECT().PutSecretValue(mock.Anything, mock.Anything).
		Return(&secretsmanager.PutSecretValueOutput{ARN: awssdk.String("arn:k")}, nil).Once()
	if _, err := c.Put(context.Background(), "k", []byte("new")); err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	_, ok := c.cache["k"]
	c.mu.RUnlock()
	if ok {
		t.Fatal("cache should have been invalidated")
	}
}

// === pure cache tests (kept) ===

// TestCacheRespectsTTL verifies the in-memory cache hides repeated
// lookups for the same secret within the TTL window. We bypass
// FromConfig (which requires a live SDK) and probe the cache code
// path directly.
func TestCache_HitsAndExpires(t *testing.T) {
	c := &Client{
		cfg:   awssdk.Config{},
		cache: map[string]cacheEntry{},
		ttl:   100 * time.Millisecond,
	}
	c.cache["foo"] = cacheEntry{value: []byte("bar"), fetched: time.Now()}

	// Within TTL: read directly returns cached.
	c.mu.RLock()
	entry, ok := c.cache["foo"]
	c.mu.RUnlock()
	if !ok || string(entry.value) != "bar" {
		t.Fatal("expected hit")
	}

	// After expiry, the cache is still populated but Get() would
	// re-fetch. Confirm staleness check.
	time.Sleep(150 * time.Millisecond)
	c.mu.RLock()
	entry, _ = c.cache["foo"]
	c.mu.RUnlock()
	if time.Since(entry.fetched) < c.ttl {
		t.Fatal("entry should be stale")
	}
}

// TestInvalidateCache verifies eviction.
func TestInvalidateCache(t *testing.T) {
	c := &Client{
		cache: map[string]cacheEntry{
			"foo": {value: []byte("bar"), fetched: time.Now()},
		},
		ttl: time.Minute,
	}
	c.InvalidateCache("foo")
	c.mu.RLock()
	_, ok := c.cache["foo"]
	c.mu.RUnlock()
	if ok {
		t.Fatal("entry should have been evicted")
	}
}

// TestCacheConcurrency exercises concurrent reads + invalidations
// to make sure the mutex pattern is correct.
func TestCacheConcurrency(t *testing.T) {
	c := &Client{
		cache: map[string]cacheEntry{},
		ttl:   time.Minute,
	}
	c.cache["k"] = cacheEntry{value: []byte("v"), fetched: time.Now()}

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c.mu.RLock()
			_ = c.cache["k"]
			c.mu.RUnlock()
		}()
		go func() {
			defer wg.Done()
			c.InvalidateCache("k")
			c.mu.Lock()
			c.cache["k"] = cacheEntry{value: []byte("v"), fetched: time.Now()}
			c.mu.Unlock()
		}()
	}
	wg.Wait()
}

// TestNoTTL_ZeroIsBypass: if ttl is 0, the cache code paths
// short-circuit (Get always re-fetches; Invalidate is a no-op).
func TestNoTTL_BypassPath(t *testing.T) {
	c := &Client{
		cache: map[string]cacheEntry{},
		ttl:   0,
	}
	c.invalidate("missing") // no-op, doesn't panic
	if len(c.cache) != 0 {
		t.Fatal("cache should remain empty")
	}
}

// silence unused-import shim
var _ = context.Background
