package aws

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestResolve_RegionFromOpts(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cfg, err := Resolve(context.Background(), Opts{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("region: got %q, want us-east-1", cfg.Region)
	}
}

func TestResolve_RegionFromEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	// Clear shared config dirs so SDK doesn't pick up dev env.
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	cfg, err := Resolve(context.Background(), Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "us-west-2" {
		t.Fatalf("region: got %q, want us-west-2", cfg.Region)
	}
}

func TestResolve_NoRegionFails(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("HOME", os.TempDir())
	_, err := Resolve(context.Background(), Opts{})
	if err == nil {
		t.Fatal("expected no-region error")
	}
	if !strings.Contains(err.Error(), "no region") {
		t.Fatalf("expected no-region error, got %v", err)
	}
}

func TestResolve_EndpointOverride(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cfg, err := Resolve(context.Background(), Opts{
		Region:   "us-east-1",
		Endpoint: "http://localhost:4566",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseEndpoint == nil || *cfg.BaseEndpoint != "http://localhost:4566" {
		t.Fatalf("BaseEndpoint not set correctly: %v", cfg.BaseEndpoint)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", " ", "x", "y") != "x" {
		t.Fatal("firstNonEmpty wrong")
	}
	if firstNonEmpty("", "") != "" {
		t.Fatal("firstNonEmpty empty case")
	}
}
