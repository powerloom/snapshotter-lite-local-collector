package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

var SlackAlertInstance *SlackAlertService

// SlackAlertService handles sending alerts to Slack via webhook
type SlackAlertService struct {
	webhookURL string
	client     *http.Client
	enabled    bool
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
		enabled: true,
	}
	log.Info("Slack alerts initialized")
}

// SendMeshAlert sends a mesh lifecycle alert to Slack
func (s *SlackAlertService) SendMeshAlert(event string, metrics MeshHealthMetrics) {
	if !s.enabled {
		return
	}

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

