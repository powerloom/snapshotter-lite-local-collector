package service

import (
	"encoding/json"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

var healthServer *HealthServer

// HealthServer provides HTTP health check endpoint
type HealthServer struct {
	server *server
	port   string
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status             string `json:"status"`     // "healthy", "degraded", "unhealthy"
	MeshState          string `json:"mesh_state"` // "healthy", "degraded", "pruned"
	MeshReady          bool   `json:"mesh_ready"` // true if mesh is healthy and ready for submissions
	DiscoveryPeers     int    `json:"discovery_peers"`
	SubmissionsPeers   int    `json:"submissions_peers"`
	TotalConnected     int    `json:"total_connected"`
	UptimeSeconds      int    `json:"uptime_seconds"`
	TotalPruningEvents int    `json:"total_pruning_events"`
	LastPruningTime    string `json:"last_pruning_time,omitempty"`
	Timestamp          string `json:"timestamp"`
}

// InitializeHealthServer initializes the health check HTTP server
func InitializeHealthServer(s *server, port string) {
	if port == "" {
		port = "8080" // Default health check port
	}

	healthServer = &HealthServer{
		server: s,
		port:   port,
	}

	http.HandleFunc("/health", healthServer.handleHealth)
	http.HandleFunc("/ready", healthServer.handleReady)

	go func() {
		addr := ":" + port
		log.Infof("Starting health check server on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Errorf("Health check server failed: %v", err)
		}
	}()
}

// handleHealth returns detailed health status
func (h *HealthServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	metrics := h.server.GetMeshHealthMetrics()

	// Determine overall status
	status := "unhealthy"
	if metrics.State == MeshStateHealthy {
		status = "healthy"
	} else if metrics.State == MeshStateDegraded {
		status = "degraded"
	}

	// Format last pruning time
	var lastPruningTime string
	if !metrics.LastPruningTime.IsZero() {
		lastPruningTime = metrics.LastPruningTime.Format(time.RFC3339)
	}

	response := HealthResponse{
		Status:             status,
		MeshState:          string(metrics.State),
		MeshReady:          metrics.State == MeshStateHealthy,
		DiscoveryPeers:     metrics.DiscoveryPeerCount,
		SubmissionsPeers:   metrics.SubmissionsPeerCount,
		TotalConnected:     metrics.TotalConnectedPeers,
		UptimeSeconds:      int(metrics.Uptime.Seconds()),
		TotalPruningEvents: metrics.TotalPruningEvents,
		LastPruningTime:    lastPruningTime,
		Timestamp:          time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")

	// Set HTTP status code based on health
	if status == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else if status == "degraded" {
		w.WriteHeader(http.StatusOK) // Still 200, but status indicates degraded
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(response)
}

// handleReady returns readiness status
// Ready as soon as the HTTP server is listening - mesh formation happens in background
// The service can accept gRPC submissions immediately, they will be queued/buffered
// until the mesh forms. This allows dependent services to start without waiting.
func (h *HealthServer) handleReady(w http.ResponseWriter, r *http.Request) {
	metrics := h.server.GetMeshHealthMetrics()

	// Service is ready immediately when HTTP server is listening
	// Mesh state doesn't block readiness - the service can accept gRPC submissions
	// regardless of mesh state. Submissions will be queued and sent when mesh forms.
	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"ready":             true,
		"mesh_state":        string(metrics.State),
		"discovery_peers":   metrics.DiscoveryPeerCount,
		"submissions_peers": metrics.SubmissionsPeerCount,
		"uptime_seconds":    int(metrics.Uptime.Seconds()),
	}
	json.NewEncoder(w).Encode(response)
}
