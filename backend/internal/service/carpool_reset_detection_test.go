package service

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func resetMonitorObservation() CarpoolResetObservation {
	now := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	return CarpoolResetObservation{ObservedAt: now, ResetAt: now.Add(3 * 24 * time.Hour), Utilization: 20}
}

func TestCarpoolResetDetection(t *testing.T) {
	for _, scenario := range []string{"baseline", "unchanged", "zero alone", "window reset zero", "window reset missed zero", "natural rollover", "jitter", "old window", "out of order", "receipt", "receipt no drop", "old receipt", "already processed receipt", "receipt after window event"} {
		t.Run(scenario, func(t *testing.T) {
			old := resetMonitorObservation()
			state := &CarpoolResetBaseline{Observation: old, Since: old.ObservedAt.Add(-time.Hour)}
			next := old
			next.ObservedAt = old.ObservedAt.Add(10 * time.Minute)
			want := ""
			switch scenario {
			case "baseline":
				state = nil
				next.Utilization = 0
			case "zero alone":
				next.Utilization = 0
			case "window reset zero":
				next.ResetAt = next.ObservedAt.Add(7*24*time.Hour - time.Minute)
				next.Utilization = 0
				want = "window"
			case "window reset missed zero":
				next.ResetAt = next.ObservedAt.Add(7*24*time.Hour - time.Minute)
				next.Utilization = .5
				want = "window"
			case "natural rollover":
				state.Observation.ResetAt = old.ObservedAt.Add(5 * time.Minute)
				next.ResetAt = state.Observation.ResetAt.Add(7 * 24 * time.Hour)
				next.Utilization = 30
				want = "window"
			case "jitter":
				next.ResetAt = old.ResetAt.Add(time.Second)
				next.Utilization = 0
			case "old window":
				next.ResetAt = old.ResetAt.Add(-24 * time.Hour)
				next.Utilization = 0
			case "out of order":
				next.ObservedAt = old.ObservedAt
				next.ResetAt = next.ObservedAt.Add(7 * 24 * time.Hour)
				next.Utilization = 0
			case "receipt", "receipt no drop", "old receipt", "already processed receipt", "receipt after window event":
				next.Receipt = &CarpoolResetReceipt{ID: 5, OccurredAt: next.ObservedAt.Add(-time.Minute)}
				next.Utilization = .5
				want = "receipt"
				if scenario == "receipt no drop" {
					next.Utilization = 20
					want = ""
				}
				if scenario == "old receipt" {
					next.Receipt.OccurredAt = state.Since.Add(-time.Minute)
					want = ""
				}
				if scenario == "already processed receipt" {
					state.ReceiptID = 5
					want = ""
				}
				if scenario == "receipt after window event" {
					state.LastResetObservedAt = next.ObservedAt
					want = ""
				}
			}
			_, reason := AdvanceCarpoolResetBaseline(state, next)
			require.Equal(t, want, reason)
		})
	}
}

func TestCarpoolResetObservationRejectsInvalidData(t *testing.T) {
	now := resetMonitorObservation().ObservedAt
	for _, scenario := range []string{"missing", "missing percent", "stale", "undated", "nan", "infinity", "negative", "wrong window", "expired", "identity mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			window := &OpenAIRateLimitWindow{UsedPercent: 20, LimitWindowSeconds: 604800, ResetAt: now.Add(time.Hour).Unix()}
			usage := &OpenAIQuotaUsage{AccountID: "shared-account", FetchedAt: now.Unix(), RateLimit: &OpenAIRateLimit{SecondaryWindow: window}}
			switch scenario {
			case "missing":
				usage.RateLimit = nil
			case "missing percent":
				window.usedPercentMissing = true
			case "stale":
				usage.FetchedAt = now.Add(-time.Hour).Unix()
			case "undated":
				usage.FetchedAt = 0
			case "nan":
				window.UsedPercent = math.NaN()
			case "infinity":
				window.UsedPercent = math.Inf(1)
			case "negative":
				window.UsedPercent = -1
			case "wrong window":
				window.LimitWindowSeconds = 18000
			case "expired":
				window.ResetAt = now.Add(-time.Minute).Unix()
			case "identity mismatch":
				usage.AccountID = "other-account"
			}
			_, ok := ReadCarpoolResetObservation(usage, "shared-account", now)
			require.False(t, ok)
		})
	}
}

func TestCarpoolResetDelayedWindowAfterReceipt(t *testing.T) {
	old := resetMonitorObservation()
	state, _ := AdvanceCarpoolResetBaseline(nil, old)
	next := old
	next.ObservedAt = old.ObservedAt.Add(10 * time.Minute)
	next.Utilization = 0
	next.Receipt = &CarpoolResetReceipt{ID: 1, OccurredAt: next.ObservedAt.Add(-30 * time.Second)}
	state, reason := AdvanceCarpoolResetBaseline(&state, next)
	require.Equal(t, "receipt", reason)
	resetAt := next.ObservedAt.Add(7*24*time.Hour - 30*time.Second)
	next.ObservedAt = next.ObservedAt.Add(10 * time.Minute)
	next.ResetAt = resetAt
	next.Utilization = 1
	state, reason = AdvanceCarpoolResetBaseline(&state, next)
	require.Empty(t, reason, "delayed window timestamp must not repeat a receipt-backed reset")
	next.ObservedAt = next.ObservedAt.Add(10 * time.Minute)
	next.ResetAt = next.ObservedAt.Add(7*24*time.Hour - time.Minute)
	_, reason = AdvanceCarpoolResetBaseline(&state, next)
	require.Equal(t, "window", reason, "a genuinely later reset is still detected")
}

func TestCarpoolResetReceiptRemainsPendingUntilDrop(t *testing.T) {
	old := resetMonitorObservation()
	state, _ := AdvanceCarpoolResetBaseline(nil, old)
	next := old
	next.ObservedAt = old.ObservedAt.Add(10 * time.Minute)
	next.Receipt = &CarpoolResetReceipt{ID: 1, OccurredAt: next.ObservedAt.Add(-time.Minute)}
	state, reason := AdvanceCarpoolResetBaseline(&state, next)
	require.Empty(t, reason)
	require.Zero(t, state.ReceiptID)
	next.ObservedAt = next.ObservedAt.Add(10 * time.Minute)
	next.Utilization = .5
	state, reason = AdvanceCarpoolResetBaseline(&state, next)
	require.Equal(t, "receipt", reason)
	require.EqualValues(t, 1, state.ReceiptID)
	next.ObservedAt = next.ObservedAt.Add(10 * time.Minute)
	next.Utilization = 0
	_, reason = AdvanceCarpoolResetBaseline(&state, next)
	require.Empty(t, reason)
}
