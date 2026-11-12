package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubscriptionCostSnapshotRejectsDifferentCycle(t *testing.T) {
	reset := time.Now()
	estimate := &SubscriptionCostEstimate{EstimateFrozen: true}
	snapshot := &SubscriptionCostSnapshot{Observation: CarpoolResetObservation{ResetAt: reset}, Estimate: estimate}
	require.Equal(t, estimate, snapshot.EstimateForWindow(reset.Add(15*time.Second)))
	require.Nil(t, snapshot.EstimateForWindow(reset.Add(time.Hour)))
	require.Nil(t, snapshot.EstimateForWindow(time.Time{}))
}

func TestSubscriptionCostManualResetWithUnchangedWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(2 * time.Hour)
	state, ok := AdvanceSubscriptionCostCycle(SubscriptionCostState{}, CarpoolResetObservation{
		ObservedAt: now, ResetAt: reset, Utilization: 100,
	}, nil, time.Time{})
	require.True(t, ok)
	require.NotNil(t, UpdateSubscriptionCostEstimate(&state, 100, 1500))
	receipt := &SubscriptionCostReset{Key: "reset", OccurredAt: now.Add(5 * time.Minute)}
	stale, ok := AdvanceSubscriptionCostCycle(state, CarpoolResetObservation{
		ObservedAt: now.Add(10 * time.Minute), ResetAt: reset, Utilization: 100,
	}, receipt, time.Time{})
	require.True(t, ok)
	require.True(t, stale.PendingReset)
	require.Nil(t, UpdateSubscriptionCostEstimate(&stale, 100, 1500))
	fresh, ok := AdvanceSubscriptionCostCycle(stale, CarpoolResetObservation{
		ObservedAt: now.Add(20 * time.Minute), ResetAt: reset, Utilization: 10,
	}, receipt, time.Time{})
	require.True(t, ok)
	require.Equal(t, receipt.OccurredAt, fresh.CycleStart)
	require.Equal(t, 1100.0, *UpdateSubscriptionCostEstimate(&fresh, 10, 110).EstimatedTotalCost)
}
