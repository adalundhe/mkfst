// Command mkfst is the operator + author CLI for the mkfst TS task
// subsystem.
//
// Usage:
//
//	mkfst serve [--config mkfst.yaml] [--listen :8443]
//	mkfst submit FILE [--server URL] [--name NAME]
//	mkfst run NAME [--server URL]
//	mkfst inspect INSTANCE [--server URL]
//	mkfst module add NAME[@VERSION] [--registry URL]
//	mkfst stack apply [--config mkfst.yaml]
//	mkfst stack list
//
// The CLI is intentionally compact for v1; richer output formatting,
// JSON-mode (--json), and watch-mode (--watch) are follow-ups.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "submit":
		cmdSubmit(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "inspect":
		cmdInspect(os.Args[2:])
	case "stack":
		if len(os.Args) < 3 {
			fatal("usage: mkfst stack <apply|list>")
		}
		switch os.Args[2] {
		case "apply":
			cmdStackApply(os.Args[3:])
		case "list":
			cmdStackList(os.Args[3:])
		default:
			fatal("unknown stack subcommand: " + os.Args[2])
		}
	case "module":
		if len(os.Args) < 3 {
			fatal("usage: mkfst module <add|list>")
		}
		switch os.Args[2] {
		case "add":
			cmdModuleAdd(os.Args[3:])
		case "list":
			cmdModuleList(os.Args[3:])
		default:
			fatal("unknown module subcommand: " + os.Args[2])
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `mkfst — TypeScript workflow server + CLI

Operator commands (run on the server host):
  mkfst serve         [--config mkfst.yaml] [--listen :8443]
  mkfst stack apply   [--config mkfst.yaml]
  mkfst stack list    [--server URL]
  mkfst module add    NAME[@VERSION]
  mkfst module list   [--server URL]

Author commands (run from the client):
  mkfst submit  FILE.ts  [--server URL] [--name NAME]
  mkfst run     NAME     [--server URL]
  mkfst inspect ID       [--server URL]
`)
}

// === client subcommands ===

func cmdSubmit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	server := fs.String("server", defaultServerURL(), "mkfst server URL")
	name := fs.String("name", "", "workflow filename hint (defaults to base of FILE)")
	cert := fs.String("cert", "", "client certificate (PEM) for mTLS")
	key := fs.String("key", "", "client key (PEM) for mTLS")
	caCert := fs.String("ca", "", "server CA bundle (PEM) for verifying https")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fatal("usage: mkfst submit FILE [--server URL]")
	}
	file := fs.Arg(0)
	body, err := os.ReadFile(file)
	if err != nil {
		fatal("read: " + err.Error())
	}
	if *name == "" {
		*name = baseName(file)
	}
	url := strings.TrimRight(*server, "/") + "/v1/workflows?name=" + httpQuoteShell(*name)
	cli := buildHTTPClient(*cert, *key, *caCert)
	resp, err := cli.Post(url, "application/typescript", bytes.NewReader(body))
	if err != nil {
		fatal("post: " + err.Error())
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	if resp.StatusCode >= 300 {
		os.Exit(2)
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	server := fs.String("server", defaultServerURL(), "mkfst server URL")
	cert := fs.String("cert", "", "client certificate (PEM) for mTLS")
	key := fs.String("key", "", "client key (PEM) for mTLS")
	caCert := fs.String("ca", "", "server CA bundle (PEM)")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fatal("usage: mkfst run NAME")
	}
	wfName := fs.Arg(0)
	url := strings.TrimRight(*server, "/") + "/v1/workflows/" + wfName + "/run"
	cli := buildHTTPClient(*cert, *key, *caCert)
	resp, err := cli.Post(url, "application/octet-stream", nil)
	if err != nil {
		fatal("post: " + err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fmt.Fprintln(os.Stderr, string(body))
		os.Exit(2)
	}
	var view struct {
		InstanceID string `json:"instanceId"`
	}
	_ = json.Unmarshal(body, &view)
	fmt.Println(view.InstanceID)
}

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	server := fs.String("server", defaultServerURL(), "mkfst server URL")
	watch := fs.Bool("watch", false, "poll until terminal")
	cert := fs.String("cert", "", "client certificate (PEM) for mTLS")
	key := fs.String("key", "", "client key (PEM) for mTLS")
	caCert := fs.String("ca", "", "server CA bundle (PEM)")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fatal("usage: mkfst inspect INSTANCE")
	}
	id := fs.Arg(0)
	url := strings.TrimRight(*server, "/") + "/v1/instances/" + id

	cli := buildHTTPClient(*cert, *key, *caCert)
	for {
		resp, err := cli.Get(url)
		if err != nil {
			fatal("get: " + err.Error())
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Println(string(body))
		if !*watch {
			return
		}
		var v struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(body, &v)
		if v.State == "completed" || v.State == "failed" || v.State == "cancelled" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// === operator subcommands ===

func cmdServe(args []string)       { runServe(args) }
func cmdStackApply(args []string)  { runStackApply(args) }
func cmdStackList(args []string)   { runStackList(args) }
func cmdModuleAdd(args []string)   { runModuleAdd(args) }
func cmdModuleList(args []string)  { runModuleList(args) }

// === HTTP client builder (with mTLS support) ===

func buildHTTPClient(certPath, keyPath, caCertPath string) *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			fatal("load client cert: " + err.Error())
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if caCertPath != "" {
		caBytes, err := os.ReadFile(caCertPath)
		if err != nil {
			fatal("read CA: " + err.Error())
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			fatal("parse CA")
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}

// === helpers ===

func defaultServerURL() string {
	if v := os.Getenv("MKFST_SERVER"); v != "" {
		return v
	}
	return "http://127.0.0.1:8443"
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func httpQuoteShell(s string) string {
	// Minimal URL-quoting for query params we control.
	out := []byte{}
	for _, r := range s {
		switch r {
		case ' ':
			out = append(out, '+')
		case '?', '&', '=', '#', '%':
			out = append(out, '%', hex(byte(r)>>4), hex(byte(r)&0xF))
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	return string(out)
}

func hex(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'A' + b - 10
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
