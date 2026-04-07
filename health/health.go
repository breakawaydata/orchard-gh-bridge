package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

// Server provides health check endpoints for Kubernetes probes.
type Server struct {
	port          int
	orchardClient orchard.Client
	logger        *slog.Logger
}

func NewServer(port int, orchardClient orchard.Client, logger *slog.Logger) *Server {
	return &Server{
		port:          port,
		orchardClient: orchardClient,
		logger:        logger.With("component", "health"),
	}
}

// Start begins listening. Blocks until the server exits.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	addr := fmt.Sprintf(":%d", s.port)
	s.logger.Info("health server listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		s.logger.Error("health server error", "error", err)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.orchardClient.Ping(ctx); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "not ready",
			"error":  err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}
