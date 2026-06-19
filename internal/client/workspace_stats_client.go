// Package client provides typed HTTP clients for inter-service communication.
package client

import "context"

// VendorStats holds per-vendor workspace statistics returned by WorkspaceStatsClient.
// When Available is false the numeric fields are all zero and the caller MUST treat
// the three derived dimensions (reliability, collab, comm) as unavailable.
type VendorStats struct {
	// Available reports whether the workspace-stats endpoint is reachable and
	// returned a valid response. When false, Reliability/Collaboration/Communication
	// are zero and the match response sets partial=true.
	Available bool

	// Reliability is the vendor's reliability score in [0.0, 1.0].
	Reliability float64

	// Collaboration is the vendor's collaboration score in [0.0, 1.0].
	Collaboration float64

	// Communication is the vendor's communication score in [0.0, 1.0].
	Communication float64
}

// WorkspaceStatsClient fetches per-vendor workspace statistics.
// Defined as an interface so the real HTTP implementation and the Noop can be swapped.
type WorkspaceStatsClient interface {
	// GetVendorStats returns workspace-sourced statistics for a vendor.
	// When the workspace-stats endpoint is unavailable the implementation MUST
	// return VendorStats{Available: false} and a nil error so callers can degrade
	// gracefully rather than treating unavailability as a hard failure.
	GetVendorStats(ctx context.Context, vendorUserID string) (VendorStats, error)
}

// NoopWorkspaceStatsClient is the ship-phase implementation of WorkspaceStatsClient.
// It always returns {Available: false} so the match engine ships with partial=true
// until a real workspace-stats endpoint is available.
type NoopWorkspaceStatsClient struct{}

// GetVendorStats implements WorkspaceStatsClient by returning {Available: false}.
// All numeric fields are zero; callers must set partial=true in the response.
func (n *NoopWorkspaceStatsClient) GetVendorStats(_ context.Context, _ string) (VendorStats, error) {
	return VendorStats{Available: false}, nil
}
