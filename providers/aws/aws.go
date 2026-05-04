// Package aws is mkfst's shared AWS credential + region resolver.
// Every AWS provider (DynamoDB, Secrets Manager, SQS, …) builds
// its SDK client on top of a `*aws.Config` produced by Resolve.
//
// Auth resolution chain:
//
//   1. If Opts.RoleARN is set, the base credential chain is
//      resolved (env → shared config → IMDS / IRSA / web identity)
//      and then used to assume that role via STS. The session's
//      temporary credentials drive every API call.
//   2. Otherwise the base credential chain is used directly. This
//      picks up:
//        - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env vars
//        - The shared credentials / config files (~/.aws/...)
//        - EC2 instance profile / EKS service-account (IRSA) /
//          ECS task role / Lambda execution role
//      whichever the host has, in standard SDK precedence order.
//
// Region resolution:
//
//   1. Opts.Region (explicit)
//   2. AWS_REGION env var
//   3. AWS_DEFAULT_REGION env var
//   4. Shared config file's region (~/.aws/config)
//
// Everything is consumed via the standard aws-sdk-go-v2
// machinery; we don't reinvent the credential chain.
package aws

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Opts configures Resolve.
type Opts struct {
	// Region overrides the chain. Empty falls through to env / shared config.
	Region string

	// RoleARN, when non-empty, triggers assume-role on top of the
	// base credential chain. The base chain provides the
	// credentials used to make the AssumeRole STS call; the
	// returned temporary credentials drive every other API call.
	RoleARN string

	// ExternalID is the optional external-ID for cross-account
	// assume-role. Required by some target accounts; ignored
	// otherwise.
	ExternalID string

	// SessionName is the assume-role session name (visible in
	// CloudTrail). Default "mkfst".
	SessionName string

	// SessionDuration is how long the assumed-role credentials
	// live before refresh. Default 1 hour. Min 15 min, max
	// dictated by the role's MaxSessionDuration policy.
	SessionDuration time.Duration

	// Endpoint optionally overrides the default service endpoint.
	// Used for LocalStack / regional endpoint pinning / VPC
	// endpoints. Applies to every API the resolved config drives.
	Endpoint string

	// MaxRetries caps SDK-level retries per API call. Default 3.
	MaxRetries int

	// HTTPTimeout caps each underlying HTTP call. Default 30s.
	HTTPTimeout time.Duration
}

// Resolve produces an aws.Config that the various provider
// clients consume. Call once per logical AWS scope (per
// Region/Role pairing); reuse the resulting config across
// providers in that scope.
func Resolve(ctx context.Context, opts Opts) (awssdk.Config, error) {
	if opts.SessionName == "" {
		opts.SessionName = "mkfst"
	}
	if opts.SessionDuration <= 0 {
		opts.SessionDuration = time.Hour
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.HTTPTimeout <= 0 {
		opts.HTTPTimeout = 30 * time.Second
	}

	loadOpts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() awssdk.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = opts.MaxRetries
			})
		}),
	}
	if opts.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(opts.Region))
	}

	baseCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return awssdk.Config{}, fmt.Errorf("aws.Resolve: load default config: %w", err)
	}

	// Late-bind region from env if SDK didn't pick one up.
	if baseCfg.Region == "" {
		if r := firstNonEmpty(os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION")); r != "" {
			baseCfg.Region = r
		}
	}

	if opts.RoleARN != "" {
		stsClient := sts.NewFromConfig(baseCfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, opts.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = opts.SessionName
			o.Duration = opts.SessionDuration
			if opts.ExternalID != "" {
				o.ExternalID = awssdk.String(opts.ExternalID)
			}
		})
		baseCfg.Credentials = awssdk.NewCredentialsCache(provider)
	}

	if opts.Endpoint != "" {
		baseCfg.BaseEndpoint = awssdk.String(opts.Endpoint)
	}

	if baseCfg.Region == "" {
		return awssdk.Config{}, errors.New("aws.Resolve: no region configured (set Opts.Region, AWS_REGION, AWS_DEFAULT_REGION, or shared config)")
	}
	return baseCfg, nil
}

// FromEnv resolves with zero options — purely from the
// environment + IMDS chain. The common path for "use whatever
// auth the host already has."
func FromEnv(ctx context.Context) (awssdk.Config, error) {
	return Resolve(ctx, Opts{})
}

// FromARN resolves with assume-role on the given ARN. Region
// inherits from the env chain.
func FromARN(ctx context.Context, roleARN, externalID string) (awssdk.Config, error) {
	return Resolve(ctx, Opts{RoleARN: roleARN, ExternalID: externalID})
}

// firstNonEmpty returns the first non-empty string from the args.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
