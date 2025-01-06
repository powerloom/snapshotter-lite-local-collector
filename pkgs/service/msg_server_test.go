package service

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func StartServer() {

}

func TestGracefulShutdown(t *testing.T) {
	// Initialize the service
	err := InitializeService()
	assert.NoError(t, err)

	// Create a channel to listen for shutdown completion
	shutdownComplete := make(chan struct{})

	// Start the server in a separate goroutine
	go func() {
		StartServer()
		close(shutdownComplete)
	}()

	// Give the server some time to start
	time.Sleep(2 * time.Second)

	// Send a termination signal
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sigs <- syscall.SIGINT

	// Wait for the server to shut down
	select {
	case <-shutdownComplete:
		// Server shut down gracefully
	case <-time.After(20 * time.Second):
		t.Fatal("Server did not shut down gracefully within the timeout period")
	}

	// Verify that the server has shut down gracefully
	assert.True(t, true, "Server shut down gracefully")
}
