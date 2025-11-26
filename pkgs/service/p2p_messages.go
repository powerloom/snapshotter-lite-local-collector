package service

import "proto-snapshot-server/pkgs"

// P2PSnapshotSubmission represents the data structure for snapshot submissions
// sent over the P2P network.
type P2PSnapshotSubmission struct {
	// EpochID is the epoch for which the submission is made.
	EpochID uint64 `json:"epoch_id"`
	// Submissions contains a list of project CIDs.
	// In the future, this could be a more complex structure if a single
	// snapshotter submits for multiple projects at once.
	Submissions []*pkgs.SnapshotSubmission `json:"submissions"`
	// SnapshotterID is the libp2p Peer ID of the submitting node.
	SnapshotterID string `json:"snapshotter_id"`
	// Signature is the cryptographic signature of the submission payload.
	Signature []byte `json:"signature"`
}
