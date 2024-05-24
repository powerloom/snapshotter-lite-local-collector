package helpers

import (
	"bytes"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"net/http"
	"proto-snapshot-server/config"
	"time"
)

type ReportingService struct {
	url    string
	client *http.Client
}

type Message struct {
	Content string `json:"content"`
}

func InitializeReportingService(url string, timeout time.Duration) *ReportingService {
	return &ReportingService{
		url: url, client: &http.Client{Timeout: timeout},
	}
}

// sendPostRequest sends a POST request to the specified URL
func (s *ReportingService) SendFailureNotification(stringData string) {
	jsonData, err := json.Marshal(&Message{Content: stringData})
	if err != nil {
		log.Errorln("Unable to marshal notification: ", stringData)
		return
	}
	req, err := http.NewRequest("POST", config.SettingsObj.PowerloomReportingUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Errorln("Error creating request: ", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Send the request
	resp, err := s.client.Do(req)
	if err != nil {
		log.Errorln("Error sending request: ", err)
		// Handle error in case of failure
		return
	}
	defer resp.Body.Close()

	// Here you can handle response or further actions
	log.Debugln("Response status: ", resp.Status)
}
