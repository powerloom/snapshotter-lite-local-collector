package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/service"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	config.LoadConfig()
	service.ConfigureRelayer()
	wg.Add(1)
	service.StartSubmissionServer(service.NewMsgServerImpl())
	wg.Wait()
}
