package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"
	"time"
)

func main() {
	var wg sync.WaitGroup
	helpers.InitLogger()
	config.LoadConfig()
	service.InitializeReportingService(config.SettingsObj.PowerloomReportingUrl, 5*time.Second)
	service.ConfigureRelayer()
	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImplV2())
	wg.Wait()
}
