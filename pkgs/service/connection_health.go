package service

import (
	"context"
	"strings"
)

// IsResourceLimitExceeded attempts to create a regular stream to check if we're hitting resource limits
func IsResourceLimitExceeded() bool {
	ctx := context.Background()
	stream, err := SequencerHostConn.NewStream(ctx, SequencerId, "/collect")
	if err != nil {
		return strings.Contains(err.Error(), "resource limit exceeded")
	}
	// Make sure to close the test stream
	defer stream.Close()
	return false
}
