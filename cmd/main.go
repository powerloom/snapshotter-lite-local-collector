package main

import (
	"os"
	"os/signal"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs/helpers"
	"proto-snapshot-server/pkgs/service"
	"sync"
	"syscall"

	log "github.com/sirupsen/logrus"
)

func main() {
	// Initiate logger
	helpers.InitLogger()

	// Load the config object
	config.LoadConfig()

	// Initialize the service
	if err := service.InitializeService(); err != nil {
		log.Errorf("Failed to initialize service: %v", err)
	}

	// Create a new submission server instance
	server := service.NewMsgServerImplV2()

	// Set up signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		service.StartSubmissionServer(server)
	}()

	// Wait for termination signal
	sig := <-sigs
	log.Infof("✅ Received signal: %s. Shutting down gracefully...", sig)

	// Perform cleanup
	service.GracefulShutdownServer(server)

	wg.Wait()
}
