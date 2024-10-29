package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"
	"time"
)

func main() {
	helpers.InitLogger()
	config.LoadConfig()
	service.InitializeReportingService(config.SettingsObj.PowerloomReportingUrl, 5*time.Second)
	service.ConfigureRelayer()

	var wg sync.WaitGroup
	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImplV2())
	wg.Wait()
}
