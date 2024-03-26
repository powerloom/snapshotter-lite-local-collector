package config

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"os"
	"strings"
)

var SettingsObj *Settings

type Settings struct {
	RelayerUrl  string `json:"RelayerUrl"`
	RelayerId   string `json:"RelayerId"`
	CollectorId string `json:"CollectorId"`
}

func LoadConfig() {
	file, err := os.Open(strings.TrimSuffix(os.Getenv("CONFIG_PATH"), "/") + "/config/settings.json")
	//file, err := os.Open("/Users/mukundrawat/powerloom/proto-snapshot-server/config/settings.json")
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

	log.Debugln("Read relayer config settings: ", config.RelayerUrl, config.RelayerId, config.CollectorId)

	SettingsObj = &config
}
