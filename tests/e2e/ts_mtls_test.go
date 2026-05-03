//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
	"mkfst/providers/ts"
	"mkfst/providers/ts/bundle"
	tsserver "mkfst/providers/ts/server"
	"mkfst/providers/workflows"
)

// TestTSServer_mTLS proves the mTLS path: server requires a
// client certificate signed by a known CA; clients without a cert
// are rejected; clients with the right cert succeed.
func TestTSServer_mTLS(t *testing.T) {
	// 1. Generate a CA + server cert + client cert.
	ca, caKey := mintCA(t, "mkfst-test-ca")
	serverCert, serverKey := mintLeaf(t, ca, caKey, "127.0.0.1", true)
	clientCert, clientKey := mintLeaf(t, ca, caKey, "client", false)

	caPool := x509.NewCertPool()
	caPool.AddCert(ca)

	// 2. TS engine.
	ctx := context.Background()
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	t.Cleanup(func() { _ = store.Close() })
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 4,
		PollInterval: 5 * time.Millisecond,
	})
	wfEng, _ := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1 << 20}),
	})
	al := bundle.NewAllowlist(tsSDKPath(t))
	_ = al.Add(bundle.ModuleEntry{Name: "@mkfst/sdk", Path: filepath.Join(tsSDKPath(t), "mkfst-sdk")})
	tsEng, _ := ts.NewEngine(ts.EngineOpts{WorkflowEngine: wfEng, Allowlist: al})

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() { _ = worker.Run(runCtx) }()

	// 3. Server with mTLS required.
	srv := tsserver.NewServer(tsEng)
	tlsServerCert := tls.Certificate{
		Certificate: [][]byte{serverCert.Raw},
		PrivateKey:  serverKey,
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsServerCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: srv.Routes(), TLSConfig: tlsCfg}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	url := "https://" + ln.Addr().String() + "/v1/workflows?name=mtls"

	// 4. Client WITHOUT a client cert → should fail.
	noCertClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool, MinVersion: tls.VersionTLS12}},
		Timeout:   5 * time.Second,
	}
	src := []byte(`import {defineTask, defineDAG} from "@mkfst/sdk";
const t = defineTask({name: "t", run: () => "ok"});
export default defineDAG("mtls", b => { b.add(t); });`)
	resp, err := noCertClient.Post(url, "application/typescript", bytes.NewReader(src))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected mTLS failure for no-cert client, got success")
	}
	if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected TLS error, got %v", err)
	}

	// 5. Client WITH the right cert → should succeed.
	tlsClientCert := tls.Certificate{
		Certificate: [][]byte{clientCert.Raw},
		PrivateKey:  clientKey,
	}
	authedClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsClientCert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS12,
			},
		},
		Timeout: 10 * time.Second,
	}
	resp, err = authedClient.Post(url, "application/typescript", bytes.NewReader(src))
	if err != nil {
		t.Fatalf("authenticated POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("authed status %d body=%s", resp.StatusCode, body)
	}
}

// === cert minting helpers ===

func mintCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, priv
}

func mintLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, priv
}

// silence unused-import warning in some build modes
var _ = pem.Block{}
