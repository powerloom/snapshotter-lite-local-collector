package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var SlackAlertInstance *SlackAlertService

// SlackAlertService handles sending alerts to Slack via webhook
type SlackAlertService struct {
	webhookURL         string
	client             *http.Client
	enabled            bool
	lastAlertTime      time.Time
	lastAlertState     MeshState
	lastAlertEvent     string
	alertThrottleMu    sync.RWMutex
	startupGracePeriod time.Duration
}

// SlackMessage represents a Slack webhook payload
type SlackMessage struct {
	Text        string       `json:"text,omitempty"`
	Username    string       `json:"username,omitempty"`
	IconEmoji   string       `json:"icon_emoji,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment represents a Slack message attachment
type Attachment struct {
	Color     string  `json:"color,omitempty"`
	Title     string  `json:"title,omitempty"`
	Text      string  `json:"text,omitempty"`
	Fields    []Field `json:"fields,omitempty"`
	Timestamp int64   `json:"ts,omitempty"`
	Footer    string  `json:"footer,omitempty"`
}

// Field represents a Slack attachment field
type Field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// InitializeSlackAlerts initializes the Slack alert service
func InitializeSlackAlerts(webhookURL string) {
	if webhookURL == "" {
		log.Info("Slack webhook URL not configured - Slack alerts disabled")
		SlackAlertInstance = &SlackAlertService{
			enabled: false,
		}
		return
	}

	SlackAlertInstance = &SlackAlertService{
		webhookURL: webhookURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		enabled:            true,
		startupGracePeriod: 5 * time.Minute, // Don't alert during first 5 minutes (startup period)
	}
	log.Info("Slack alerts initialized")
}

// SendMeshAlert sends a mesh lifecycle alert to Slack with throttling
func (s *SlackAlertService) SendMeshAlert(event string, metrics MeshHealthMetrics) {
	if !s.enabled {
		return
	}

	// Check if we should throttle this alert
	if !s.shouldSendAlert(event, metrics) {
		return
	}

	// Update last alert tracking
	s.alertThrottleMu.Lock()
	s.lastAlertTime = time.Now()
	s.lastAlertState = metrics.State
	s.lastAlertEvent = event
	s.alertThrottleMu.Unlock()

	var color string
	var emoji string
	var severity string

	switch metrics.State {
	case MeshStatePruned:
		color = "danger"
		emoji = "🚨"
		severity = "CRITICAL"
	case MeshStateDegraded:
		color = "warning"
		emoji = "⚠️"
		severity = "WARNING"
	case MeshStateHealthy:
		color = "good"
		emoji = "✅"
		severity = "INFO"
	}

	// Format uptime
	uptimeHours := int(metrics.Uptime.Hours())
	uptimeMinutes := int(metrics.Uptime.Minutes()) % 60
	uptimeStr := fmt.Sprintf("%dh %dm", uptimeHours, uptimeMinutes)

	// Format last pruning time
	var lastPruningStr string
	if !metrics.LastPruningTime.IsZero() {
		lastPruningStr = metrics.LastPruningTime.Format(time.RFC3339)
	} else {
		lastPruningStr = "Never"
	}

	title := fmt.Sprintf("%s Gossipsub Mesh Alert: %s", emoji, event)
	text := fmt.Sprintf("*State:* %s\n*Severity:* %s", metrics.State, severity)

	// Format last disconnection time
	var lastDisconnectStr string
	if !metrics.LastDisconnectionTime.IsZero() {
		lastDisconnectStr = metrics.LastDisconnectionTime.Format(time.RFC3339)
	} else {
		lastDisconnectStr = "None"
	}

	// Format peer IDs (truncate if too many)
	meshPeerIDsStr := "None"
	if len(metrics.MeshPeerIDs) > 0 {
		if len(metrics.MeshPeerIDs) <= 5 {
			meshPeerIDsStr = fmt.Sprintf("%v", metrics.MeshPeerIDs)
		} else {
			meshPeerIDsStr = fmt.Sprintf("%d peers (showing first 5): %v", len(metrics.MeshPeerIDs), metrics.MeshPeerIDs[:5])
		}
	}

	connectedPeerIDsStr := "None"
	if len(metrics.ConnectedPeerIDs) > 0 {
		if len(metrics.ConnectedPeerIDs) <= 5 {
			connectedPeerIDsStr = fmt.Sprintf("%v", metrics.ConnectedPeerIDs)
		} else {
			connectedPeerIDsStr = fmt.Sprintf("%d peers (showing first 5): %v", len(metrics.ConnectedPeerIDs), metrics.ConnectedPeerIDs[:5])
		}
	}

	fields := []Field{
		{Title: "Mesh State", Value: string(metrics.State), Short: true},
		{Title: "Severity", Value: severity, Short: true},
		{Title: "Discovery Peers", Value: fmt.Sprintf("%d", metrics.DiscoveryPeerCount), Short: true},
		{Title: "Submissions Peers", Value: fmt.Sprintf("%d", metrics.SubmissionsPeerCount), Short: true},
		{Title: "Total Connected", Value: fmt.Sprintf("%d", metrics.TotalConnectedPeers), Short: true},
		{Title: "Consecutive Low", Value: fmt.Sprintf("%d", metrics.ConsecutiveLowPeerCounts), Short: true},
		{Title: "Total Pruning Events", Value: fmt.Sprintf("%d", metrics.TotalPruningEvents), Short: true},
		{Title: "Last Pruning", Value: lastPruningStr, Short: true},
		{Title: "Uptime", Value: uptimeStr, Short: true},
		{Title: "Event", Value: event, Short: false},
		// Connection state diagnostics
		{Title: "Connection Manager", Value: fmt.Sprintf("LowWater: %d, HighWater: %d", metrics.ConnectionManagerLowWater, metrics.ConnectionManagerHighWater), Short: true},
		{Title: "Recent Disconnections", Value: fmt.Sprintf("%d total (%d we initiated, %d peer initiated)", metrics.RecentDisconnections, metrics.RecentDisconnectionsWeInitiated, metrics.RecentDisconnectionsPeerInitiated), Short: false},
		{Title: "Last Disconnection", Value: fmt.Sprintf("%s (%s)", lastDisconnectStr, metrics.LastDisconnectionDirection), Short: true},
		{Title: "Peer Tag Status", Value: metrics.PeerTagStatus, Short: false},
		{Title: "Mesh Peer IDs", Value: meshPeerIDsStr, Short: false},
		{Title: "All Connected Peer IDs", Value: connectedPeerIDsStr, Short: false},
	}

	attachment := Attachment{
		Color:     color,
		Title:     title,
		Text:      text,
		Fields:    fields,
		Timestamp: time.Now().Unix(),
		Footer:    "Local Collector Mesh Monitor",
	}

	message := SlackMessage{
		Username:    "Local Collector",
		IconEmoji:   ":satellite_antenna:",
		Attachments: []Attachment{attachment},
	}

	s.sendMessage(message)
}

// sendMessage sends a message to Slack
func (s *SlackAlertService) sendMessage(message SlackMessage) {
	jsonData, err := json.Marshal(message)
	if err != nil {
		log.Errorf("Failed to marshal Slack message: %v", err)
		return
	}

	req, err := http.NewRequest("POST", s.webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Errorf("Failed to create Slack request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		log.Errorf("Failed to send Slack alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("Slack webhook returned non-200 status: %d", resp.StatusCode)
		return
	}

	log.Debugf("Slack alert sent successfully: %s", message.Attachments[0].Title)
}

// shouldSendAlert determines if an alert should be sent based on throttling rules
func (s *SlackAlertService) shouldSendAlert(event string, metrics MeshHealthMetrics) bool {
	s.alertThrottleMu.RLock()
	defer s.alertThrottleMu.RUnlock()

	now := time.Now()

	// Always send alerts on good state transitions (mesh forming/recovering), even during startup
	// These are important indicators that the mesh is working correctly
	if event == "mesh_state_transition:pruned->healthy" ||
		event == "mesh_state_transition:pruned->degraded" ||
		event == "mesh_state_transition:degraded->healthy" {
		return true
	}

	// For bad transitions (mesh degrading), suppress during startup grace period
	// but allow after startup to catch real issues
	if event == "mesh_state_transition:healthy->pruned" ||
		event == "mesh_state_transition:degraded->pruned" ||
		event == "mesh_state_transition:healthy->degraded" {
		if metrics.Uptime < s.startupGracePeriod {
			log.Debugf("Suppressing bad state transition alert during startup grace period (uptime: %v)", metrics.Uptime)
			return false
		}
		return true
	}

	// Suppress other alerts during startup grace period (first 5 minutes)
	if metrics.Uptime < s.startupGracePeriod {
		log.Debugf("Suppressing alert during startup grace period (uptime: %v)", metrics.Uptime)
		return false
	}

	// For zero-peer publish attempts, only alert if we haven't alerted recently
	if event == "zero_peer_publish_attempt" {
		if now.Sub(s.lastAlertTime) < 5*time.Minute {
			log.Debugf("Throttling zero-peer publish alert (last alert: %v ago)", now.Sub(s.lastAlertTime))
			return false
		}
		return true
	}

	// For degraded state alerts, only send if:
	// 1. State changed since last alert, OR
	// 2. It's been at least 2 minutes since last alert for this state
	if metrics.State == MeshStateDegraded {
		if metrics.State != s.lastAlertState {
			return true // State changed
		}
		if now.Sub(s.lastAlertTime) < 2*time.Minute {
			log.Debugf("Throttling degraded state alert (last alert: %v ago)", now.Sub(s.lastAlertTime))
			return false
		}
		return true
	}

	// For pruned state, only send if:
	// 1. State changed since last alert, OR
	// 2. It's been at least 60 seconds since last alert for this state
	if metrics.State == MeshStatePruned {
		if metrics.State != s.lastAlertState {
			return true // State changed
		}
		if now.Sub(s.lastAlertTime) < 60*time.Second {
			log.Debugf("Throttling pruned state alert (last alert: %v ago)", now.Sub(s.lastAlertTime))
			return false
		}
		return true
	}

	// For recovery events, always send
	if event == "mesh_recovered" {
		return true
	}

	// Default: don't send if we've alerted recently for the same state
	if metrics.State == s.lastAlertState && now.Sub(s.lastAlertTime) < 5*time.Minute {
		log.Debugf("Throttling alert for same state (last alert: %v ago)", now.Sub(s.lastAlertTime))
		return false
	}

	return true
}
