package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

const CarpoolResetEventLifetime = 30 * time.Minute

type CarpoolResetReceipt struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
}

type CarpoolResetObservation struct {
	ObservedAt  time.Time            `json:"observed_at"`
	ResetAt     time.Time            `json:"reset_at"`
	Utilization float64              `json:"utilization"`
	Receipt     *CarpoolResetReceipt `json:"-"`
}

type CarpoolResetBaseline struct {
	Observation         CarpoolResetObservation `json:"observation"`
	Since               time.Time               `json:"since"`
	ReceiptID           int64                   `json:"receipt_id"`
	LastResetObservedAt time.Time               `json:"last_reset_observed_at"`
}

func CarpoolResetIdentityHash(identity string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(identity)))
	return hex.EncodeToString(sum[:])
}

func CarpoolResetEventKey(source CarpoolResetSource, observation CarpoolResetObservation, reason string) string {
	evidence := observation.ResetAt.Unix()
	if reason == "receipt" && observation.Receipt != nil {
		evidence = observation.Receipt.ID
	}
	return CarpoolResetIdentityHash(fmt.Sprintf("%d:%d:%d:%s:%s:%d", source.GroupID, source.AccountID, source.Revision, source.IdentityHash, reason, evidence))
}

func ReadCarpoolResetObservation(usage *OpenAIQuotaUsage, expectedAccount string, now time.Time) (CarpoolResetObservation, bool) {
	if usage == nil || usage.RateLimit == nil || usage.FetchedAt <= 0 || strings.TrimSpace(expectedAccount) == "" {
		return CarpoolResetObservation{}, false
	}
	if usage.AccountID != "" && strings.TrimSpace(usage.AccountID) != strings.TrimSpace(expectedAccount) {
		return CarpoolResetObservation{}, false
	}
	observed := time.Unix(usage.FetchedAt, 0).UTC()
	if observed.Before(now.Add(-2*time.Minute)) || observed.After(now.Add(time.Minute)) {
		return CarpoolResetObservation{}, false
	}
	var result CarpoolResetObservation
	found := false
	for _, window := range []*OpenAIRateLimitWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if window == nil || window.LimitWindowSeconds != 604800 {
			continue
		}
		if found || window.usedPercentMissing || !validSharedUsageNumber(window.UsedPercent) {
			return CarpoolResetObservation{}, false
		}
		resetAt := time.Unix(window.ResetAt, 0).UTC()
		if window.ResetAt <= 0 && window.ResetAfterSeconds > 0 && window.ResetAfterSeconds <= 604800 {
			resetAt = observed.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
		}
		if !resetAt.After(now) || resetAt.After(now.Add(7*24*time.Hour+time.Minute)) {
			return CarpoolResetObservation{}, false
		}
		result = CarpoolResetObservation{ObservedAt: observed, ResetAt: resetAt, Utilization: window.UsedPercent}
		found = true
	}
	return result, found
}

// AdvanceCarpoolResetBaseline never treats a percentage drop alone as proof.
// Receipt watermarks and monotonically advancing windows reject replayed evidence.
func AdvanceCarpoolResetBaseline(previous *CarpoolResetBaseline, next CarpoolResetObservation) (CarpoolResetBaseline, string) {
	if previous == nil {
		initial := CarpoolResetBaseline{Observation: next, Since: next.ObservedAt}
		if next.Receipt != nil {
			initial.ReceiptID = next.Receipt.ID
		}
		return initial, ""
	}
	if !next.ObservedAt.After(previous.Observation.ObservedAt) || next.ResetAt.Before(previous.Observation.ResetAt.Add(-time.Minute)) {
		return *previous, ""
	}
	if next.ResetAt.Before(previous.Observation.ResetAt) {
		next.ResetAt = previous.Observation.ResetAt
	}
	result := *previous
	result.Observation = next
	start := next.ResetAt.Add(-7 * 24 * time.Hour)
	reason := ""
	if next.ResetAt.After(previous.Observation.ResetAt.Add(time.Minute)) &&
		!start.Before(previous.Observation.ObservedAt.Add(-time.Minute)) && !start.After(next.ObservedAt.Add(time.Minute)) &&
		start.After(previous.LastResetObservedAt) {
		reason = "window"
	}
	if receipt := next.Receipt; receipt != nil && receipt.ID > previous.ReceiptID {
		fresh := receipt.OccurredAt.After(previous.Since) && receipt.OccurredAt.After(previous.LastResetObservedAt) &&
			!receipt.OccurredAt.Before(next.ObservedAt.Add(-CarpoolResetEventLifetime)) && !receipt.OccurredAt.After(next.ObservedAt)
		if fresh && previous.Observation.Utilization > 0 && next.Utilization < previous.Observation.Utilization &&
			next.Utilization <= math.Max(1, previous.Observation.Utilization*.1) && reason == "" {
			reason = "receipt"
		}
		// Keep fresh, not-yet-visible receipts pending while upstream usage settles.
		if !fresh || reason != "" {
			result.ReceiptID = receipt.ID
		}
	}
	if reason != "" {
		result.LastResetObservedAt = next.ObservedAt
	}
	return result, reason
}
