package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
)

// PullOption mutates the assembled pull request just before submission.
type PullOption func(*pullState)

type pullState struct {
	opts image.PullOptions
}

// Pull fetches an image (or all tags of a repository, when WithAllTags is
// set) from a registry. Returns an Event channel that streams Status and
// Progress events for each layer; closes with EventDone on success or
// EventError on failure.
//
// ref is the image reference in the standard "[registry/]repo[:tag]"
// shape — e.g. "alpine:3.19", "ghcr.io/owner/img:sha-abc123".
//
// Goroutine ownership: a streamer goroutine is spawned to decode the
// daemon's jsonmessage stream into the channel. Callers MUST drain to
// close OR cancel ctx — failure to do either leaks the streamer + the
// underlying HTTP response body. See Client.Build's doc for the same
// guarantee.
func (c *Client) Pull(ctx context.Context, ref string, opts ...PullOption) (<-chan Event, error) {
	state := &pullState{}
	for _, opt := range opts {
		opt(state)
	}

	rc, err := c.api.ImagePull(ctx, ref, state.opts)
	if err != nil {
		return nil, fmt.Errorf("docker.Pull: %w", err)
	}

	events := make(chan Event, 16)
	go func() {
		defer rc.Close()
		streamJSONMessages(ctx, rc, events)
	}()
	return events, nil
}

// === Pull options ===

// AllTags pulls every tag of the named repository. Without this, only the
// specific tag (or :latest if none specified) is pulled.
func AllTags() PullOption {
	return func(s *pullState) { s.opts.All = true }
}

// PullPlatform pins the platform of the manifest to fetch — same shape as
// BuildPlatform ("linux/amd64", "linux/arm64", …). Useful when running
// cross-arch (a Linux daemon pulling an ARM image to feed buildx, etc.).
func PullPlatform(platform string) PullOption {
	return func(s *pullState) { s.opts.Platform = platform }
}

// WithRegistryAuth provides credentials for a private registry. The auth is
// JSON-encoded and base64'd in the form the daemon expects (the SDK leaves
// this encoding to the caller). Use AuthConfig + PullAuth for the friendly
// path; this option is the literal-string escape for callers that already
// have an encoded blob (e.g. from `docker login` keychain).
func WithRegistryAuth(encodedAuth string) PullOption {
	return func(s *pullState) { s.opts.RegistryAuth = encodedAuth }
}

// PullAuth is the friendly registry-auth option — accepts an AuthConfig
// (the same struct used by RegistryAuth on the build side), JSON-encodes
// it, and base64s it for the daemon. Prefer this over WithRegistryAuth.
func PullAuth(auth AuthConfig) PullOption {
	return func(s *pullState) {
		encoded, err := encodeAuth(auth)
		if err != nil {
			// Encoding only fails on a programmer error (truly weird input);
			// surface as a no-op rather than panic, and let the daemon
			// reject the unauthenticated request.
			return
		}
		s.opts.RegistryAuth = encoded
	}
}

// PullPrivilegeFunc supplies a callback that's invoked when the daemon
// returns 401. The callback returns a fresh base64-encoded auth header to
// retry with — useful for rotating short-lived tokens (ECR, ACR, etc.).
func PullPrivilegeFunc(fn func(context.Context) (string, error)) PullOption {
	return func(s *pullState) { s.opts.PrivilegeFunc = fn }
}

// encodeAuth serializes auth in the daemon's expected format: a JSON object
// of registry.AuthConfig fields, base64-encoded with URL-safe encoding (the
// docker CLI uses URL-safe base64; using std encoding works but trips up
// older daemons).
func encodeAuth(auth AuthConfig) (string, error) {
	cfg := registry.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Password,
		Email:         auth.Email,
		Auth:          auth.Auth,
		IdentityToken: auth.IdentityToken,
		RegistryToken: auth.RegistryToken,
		ServerAddress: auth.ServerAddress,
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}
