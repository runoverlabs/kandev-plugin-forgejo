// Package resttest is a scripted Forgejo instance for tests: handlers keyed by
// method and path, with every request and Authorization header recorded.
package resttest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Server is a fake Forgejo instance. An unscripted method and path answers
// 404, as the instance does for an endpoint it does not serve.
type Server struct {
	server   *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []*http.Request
	authSeen []string
}

// NewServer starts a server that is closed when t ends.
func NewServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{handlers: map[string]http.HandlerFunc{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		handler, ok := s.handlers[r.Method+" "+r.URL.Path]
		s.requests = append(s.requests, r)
		s.authSeen = append(s.authSeen, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// Handle scripts a JSON response for one method and path.
func (s *Server) Handle(method, path string, status int, body any) {
	s.HandleFunc(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}

// HandleFunc scripts a custom handler, e.g. to assert on the request body.
func (s *Server) HandleFunc(method, path string, handler http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method+" "+path] = handler
}

// URL is the server's base URL.
func (s *Server) URL() string { return s.server.URL }

// Requests is every request received so far, in order.
func (s *Server) Requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// AuthSeen is the Authorization header of every request so far, in order.
func (s *Server) AuthSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

// Count reports how many requests reached method and path.
func (s *Server) Count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Method == method && r.URL.Path == path {
			n++
		}
	}
	return n
}
