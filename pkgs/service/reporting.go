package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"net/http"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"strconv"
	"time"
)

var ReportingInstance *ReportingService

type ReportingService struct {
	url    string
	client *http.Client
}
type SnapshotterIssue struct {
	InstanceID      string `json:"instanceID"`
	IssueType       string `json:"issueType"`
	ProjectID       string `json:"projectID"`
	EpochID         string `json:"epochId"`
	TimeOfReporting string `json:"timeOfReporting"`
	Extra           string `json:"extra"`
}

func InitializeReportingService(url string, timeout time.Duration) {
	ReportingInstance = &ReportingService{
		url: url + "/reportIssue", client: &http.Client{Timeout: timeout},
	}
}

// sendPostRequest sends a POST request to the specified URL
func (s *ReportingService) SendFailureNotification(request *pkgs.Request, extraData string) {
	var issue SnapshotterIssue
	if request == nil {
		issue = SnapshotterIssue{
			InstanceID:      config.SettingsObj.SignerAccountAddress,
			IssueType:       "RELAYER_CONNECTION_FAILURE", // Assume you have a constant or enum equivalent
			ProjectID:       "",
			EpochID:         "0", // Convert uint64 or similar to string
			TimeOfReporting: strconv.FormatInt(time.Now().Unix(), 10),
			Extra:           fmt.Sprintf(`{"issueDetails":"Error : %s"}`, extraData),
		}
	} else {
		issue = SnapshotterIssue{
			InstanceID:      config.SettingsObj.SignerAccountAddress,
			IssueType:       "RELAYER_CONNECTION_FAILURE", // Assume you have a constant or enum equivalent
			ProjectID:       request.ProjectId,
			EpochID:         strconv.FormatUint(request.EpochId, 10), // Convert uint64 or similar to string
			TimeOfReporting: strconv.FormatInt(time.Now().Unix(), 10),
			Extra:           fmt.Sprintf(`{"issueDetails":"Error : %s"}`, extraData),
		}
	}

	jsonData, err := json.Marshal(issue)
	if err != nil {
		log.Errorln("Unable to marshal notification for request: ", request.String())
		return
	}
	req, err := http.NewRequest("POST", s.url, bytes.NewBuffer(jsonData))
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
	log.Debugln("Reporting service response status: ", resp.Status)
}
