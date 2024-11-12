package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"

	log "github.com/sirupsen/logrus"
)

func main() {
	helpers.InitLogger()
	config.LoadConfig()

	if err := service.InitializeService(); err != nil {
		log.Fatalf("Failed to initialize service: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImplV2())
	wg.Wait()
}
