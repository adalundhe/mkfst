// Package server is the mkfst HTTP server: receives TS workflow
// submissions, runs them, exposes /v1 endpoints.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"mkfst/providers/ts"
	"mkfst/providers/workflows"
)

// Server is the HTTP API endpoint.
type Server struct {
	tsEngine    *ts.Engine
	stackLister StackLister
	submitLimit *RateLimiter

	mu sync.RWMutex
}

// SetSubmitRateLimit installs a per-source rate limit on the
// /v1/workflows POST endpoint. Pass nil to remove.
func (s *Server) SetSubmitRateLimit(rl *RateLimiter) {
	s.mu.Lock()
	s.submitLimit = rl
	s.mu.Unlock()
}

// StackLister surfaces the operator's currently-applied stacks.
// Optional — a server can be constructed without one and the
// /v1/stacks endpoint just returns an empty list.
type StackLister func() []StackInfo

// StackInfo is one row in /v1/stacks.
type StackInfo struct {
	Name     string                  `json:"name"`
	State    string                  `json:"state"`
	Services map[string]ServiceInfo  `json:"services"`
}

// ServiceInfo is one service row.
type ServiceInfo struct {
	Image    string `json:"image"`
	Replicas int    `json:"replicas"`
	Healthy  bool   `json:"healthy"`
}

// NewServer constructs a Server bound to the given TS engine.
func NewServer(tsEng *ts.Engine) *Server {
	return &Server{tsEngine: tsEng}
}

// SetStackLister wires a stack lister for the /v1/stacks endpoint.
func (s *Server) SetStackLister(l StackLister) {
	s.mu.Lock()
	s.stackLister = l
	s.mu.Unlock()
}

// Routes returns an http.Handler with all /v1 endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workflows", s.handleWorkflows)
	mux.HandleFunc("/v1/workflows/", s.handleWorkflowOps)
	mux.HandleFunc("/v1/instances/", s.handleInstance)
	mux.HandleFunc("/v1/stacks", s.handleStacks)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// === handlers ===

// POST /v1/workflows — body: TS source bytes; query: ?name=…
func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		// Per-source rate limit on submissions: bundling is
		// CPU-bound and could DOS the server even if submitted
		// content is rejected.
		s.mu.RLock()
		rl := s.submitLimit
		s.mu.RUnlock()
		if rl != nil {
			ip := extractSourceIP(r, false)
			if !rl.Allow(ip) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED",
					"per-source submission rate limit exceeded")
				return
			}
		}
		s.submitWorkflow(w, r)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"workflows": []string{}})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD", "method not allowed")
	}
}

func (s *Server) submitWorkflow(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "READ", err.Error())
		return
	}
	defer r.Body.Close()
	filename := r.URL.Query().Get("name")
	if filename == "" {
		filename = "workflow.ts"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	wf, err := s.tsEngine.Submit(ctx, body, filename)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "SUBMIT", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    wf.Name,
		"sha256":  wf.Bundle.SHA256,
		"sizeKB":  wf.Bundle.SizeKB,
		"tasks":   len(wf.DAG.Tasks),
		"nodes":   len(wf.DAG.Nodes),
	})
}

// /v1/workflows/{name}/run, /v1/workflows/{name} (GET inspect)
func (s *Server) handleWorkflowOps(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[len("/v1/workflows/"):]
	if path == "" {
		writeError(w, http.StatusNotFound, "NOTFOUND", "workflow name missing")
		return
	}
	// /name/run
	if isRunPath(path) {
		name := path[:len(path)-len("/run")]
		s.runWorkflow(w, r, name)
		return
	}
	writeError(w, http.StatusNotFound, "NOTFOUND", "unknown sub-path")
}

func isRunPath(p string) bool {
	const suffix = "/run"
	if len(p) <= len(suffix) {
		return false
	}
	return p[len(p)-len(suffix):] == suffix
}

func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD", "method not allowed")
		return
	}
	var input []byte
	if r.ContentLength > 0 {
		input, _ = io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	id, err := s.tsEngine.Run(ctx, name, input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "RUN", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"instanceId": id})
}

// /v1/stacks — list applied stacks (operator info).
func (s *Server) handleStacks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD", "method not allowed")
		return
	}
	s.mu.RLock()
	lister := s.stackLister
	s.mu.RUnlock()
	if lister == nil {
		writeJSON(w, http.StatusOK, map[string]any{"stacks": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": lister()})
}

// /v1/instances/{id}
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/instances/"):]
	if id == "" {
		writeError(w, http.StatusNotFound, "NOTFOUND", "instance id missing")
		return
	}
	info, err := s.tsEngine.Inspect(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOTFOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instanceView(info))
}

// === view types ===

type instanceJSON struct {
	ID         string                 `json:"id"`
	Definition string                 `json:"definition"`
	State      string                 `json:"state"`
	StartedAt  time.Time              `json:"startedAt"`
	EndedAt    time.Time              `json:"endedAt,omitempty"`
	Nodes      map[string]nodeJSON    `json:"nodes"`
}

type nodeJSON struct {
	State     string `json:"state"`
	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

func instanceView(info workflows.InstanceInfo) instanceJSON {
	out := instanceJSON{
		ID:         info.ID,
		Definition: info.Definition,
		State:      string(info.State),
		StartedAt:  info.StartedAt,
		EndedAt:    info.EndedAt,
		Nodes:      map[string]nodeJSON{},
	}
	for name, n := range info.Nodes {
		out.Nodes[name] = nodeJSON{
			State:     string(n.State),
			Attempts:  n.Attempts,
			LastError: n.LastError,
		}
	}
	return out
}

// === helpers ===

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": msg,
	})
}

// silence unused-import shim
var _ = errors.New
var _ = fmt.Sprintf
