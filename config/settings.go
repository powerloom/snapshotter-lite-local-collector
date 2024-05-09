package config

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"os"
	"strings"
)

var SettingsObj *Settings

type Settings struct {
	SequencerId            string `json:"SequencerId"`
	RelayerRendezvousPoint string `json:"RelayerRendezvousPoint"`
	ClientRendezvousPoint  string `json:"ClientRendezvousPoint"`
	DashboardEnabled       string `json:"DashboardEnabled"`
}

func LoadConfig() {
	file, err := os.Open(strings.TrimSuffix(os.Getenv("CONFIG_PATH"), "/") + "/config/settings.json")
	// file, err := os.Open("/Users/sulejmansarajlija/IdeaProjects/powerloom/snapshotter_lite_exp/submission-sequencer/config/settings.json")
	if err != nil {
		log.Fatalf("Failed to open config file: %v", err)
	}
	defer func(file *os.File) {
		err = file.Close()
		if err != nil {
			log.Errorf("Unable to close file: %s", err.Error())
		}
	}(file)

	decoder := json.NewDecoder(file)
	config := Settings{}
	err = decoder.Decode(&config)
	if err != nil {
		log.Fatalf("Failed to decode config file: %v", err)
	}

	SettingsObj = &config
}
