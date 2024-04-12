package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	helpers.InitLogger()
	config.LoadConfig()
	service.ConfigureRelayer()
	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImpl())
	wg.Wait()
}
