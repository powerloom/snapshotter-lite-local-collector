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
				// Check both basic connection status and resource limits
				if service.SequencerHostConn.Network().Connectedness(service.SequencerId) != network.Connected ||
					service.IsResourceLimitExceeded() {

					log.Warn("Connection issue or resource limit exceeded. Attempting atomic reset...")

					if err := service.AtomicConnectionReset(); err != nil {
						log.Errorf("Failed to perform atomic reset: %v", err)
					} else {
						log.Info("Successfully completed atomic reset")
					}
				}
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	go service.StartSubmissionServer(service.NewMsgServerImplV2())
	wg.Wait()
}
