package service

import (
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	HealthFilePath = "/tmp/sequencer-connection-healthy"
)

func createHealthFile() {
	file, err := os.Create(HealthFilePath)
	if err != nil {
		log.Errorf("Failed to create health check file: %v", err)
		return
	}
	defer file.Close()

	// Write timestamp
	timestamp := time.Now().UTC().Format(time.RFC3339)
	if _, err := file.WriteString(timestamp); err != nil {
		log.Errorf("Failed to write to health check file: %v", err)
	}
}

func removeHealthFile() {
	err := os.Remove(HealthFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			// Only log if there's an error other than file not existing
			log.Debugf("Failed to remove health check file: %v", err)
		} else {
			log.Debugf("Removal of health check file: Health check file does not exist")
		}
		// File doesn't exist is not an error condition - health check will show unhealthy anyway

	}
}

func updateHealthStatus(healthy bool) {
	if healthy {
		createHealthFile()
	} else {
		removeHealthFile()
	}
}
