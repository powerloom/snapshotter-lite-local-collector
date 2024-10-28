package main

import (
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	log "github.com/sirupsen/logrus"
)

func main() {
	var wg sync.WaitGroup
	helpers.InitLogger()
	config.LoadConfig()
	service.InitializeReportingService(config.SettingsObj.PowerloomReportingUrl, 5*time.Second)
	service.ConfigureRelayer()

	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Check connection and attempt to create a test stream
				if service.SequencerHostConn.Network().Connectedness(service.SequencerId) != network.Connected ||
					service.IsResourceLimitExceeded() {
					log.Warn("Lost connection to sequencer or resource limit exceeded. Attempting to reconnect...")
					if err := service.ConnectToSequencer(); err != nil {
						log.Errorf("Failed to reconnect to sequencer: %v", err)
					} else {
						log.Info("Successfully reconnected to sequencer")
					}
				}
			}
		}
	}()

	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImplV2())
	wg.Wait()
}
