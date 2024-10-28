package service

import (
	"context"
	"strings"

	"github.com/libp2p/go-libp2p/core/network"
)

// IsResourceLimitExceeded attempts to create a test stream to check if we're hitting resource limits
func IsResourceLimitExceeded() bool {
	ctx := context.Background()
	_, err := SequencerHostConn.NewStream(
		network.WithUseTransient(ctx, "health-check"),
		SequencerId,
		"/collect.peer",
	)
	if err != nil {
		return strings.Contains(err.Error(), "resource limit exceeded")
	}
	return false
}
