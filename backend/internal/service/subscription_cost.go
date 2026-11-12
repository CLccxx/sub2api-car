package service

import (
	"context"
	"math"
	"strings"
	"time"
)

// SubscriptionCostEstimate deliberately separates the subscription allowance
// from credit spend. Only EstimatedTotalCost may feed carpool limits.
type SubscriptionCostEstimate struct {
	EstimatedTotalCost *float64 `json:"estimated_total_cost"`
	CreditsUsedCost    *float64 `json:"credits_used_cost"`
	EstimateFrozen     bool     `json:"estimate_frozen"`
	EstimateSource     string   `json:"estimate_source,omitempty"`
}

type SubscriptionCostReset struct {
	Key        string
	OccurredAt time.Time
}

type SubscriptionCostState struct {
	Observation  CarpoolResetObservation   `json:"observation"`
	CycleStart   time.Time                 `json:"cycle_start"`
	ResetKey     string                    `json:"reset_key"`
	PendingReset bool                      `json:"pending_reset"`
	SampleSlot   time.Time                 `json:"sample_slot"`
	Estimate     *SubscriptionCostEstimate `json:"estimate,omitempty"`
	FrozenBase   *float64                  `json:"frozen_base,omitempty"`
	Source       string                    `json:"source,omitempty"`
}

// AdvanceSubscriptionCostCycle uses independent account state: changes to group
// membership never unlock a frozen allowance. A receipt clears the old cycle
// but requires a fresh below-full sample before a same-window reset settles.
func AdvanceSubscriptionCostCycle(previous SubscriptionCostState, next CarpoolResetObservation, receipt *SubscriptionCostReset, identityStart time.Time) (SubscriptionCostState, bool) {
	if !previous.Observation.ObservedAt.IsZero() && (next.ObservedAt.Before(previous.Observation.ObservedAt) || next.ResetAt.Before(previous.Observation.ResetAt.Add(-time.Minute))) {
		return previous, false
	}
	state := previous
	if state.CycleStart.IsZero() {
		state.CycleStart = next.ResetAt.Add(-7 * 24 * time.Hour)
		if identityStart.After(state.CycleStart) {
			state.CycleStart = identityStart
		}
	} else if next.ResetAt.After(previous.Observation.ResetAt.Add(time.Minute)) {
		start := next.ResetAt.Add(-7 * 24 * time.Hour)
		if start.Before(previous.Observation.ObservedAt.Add(-time.Minute)) || start.After(next.ObservedAt.Add(time.Minute)) {
			return previous, false
		}
		state = SubscriptionCostState{CycleStart: start, ResetKey: previous.ResetKey}
	} else {
		next.ResetAt = previous.Observation.ResetAt
	}
	if receipt != nil && receipt.Key != state.ResetKey && !receipt.OccurredAt.After(next.ObservedAt) {
		state.ResetKey = receipt.Key
		if receipt.OccurredAt.After(state.CycleStart) {
			state.CycleStart = receipt.OccurredAt
			state.FrozenBase = nil
			state.Estimate = nil
			state.Source = ""
			state.PendingReset = true
		}
	}
	if state.PendingReset && next.Utilization < 100 && next.ObservedAt.After(state.CycleStart) {
		state.PendingReset = false
	}
	state.Observation = next
	return state, true
}

func UpdateSubscriptionCostEstimate(state *SubscriptionCostState, utilization, cost float64) *SubscriptionCostEstimate {
	if state == nil || state.PendingReset || !validSharedUsageNumber(cost) || !validSharedUsageNumber(utilization) {
		return nil
	}
	if state.FrozenBase == nil {
		if utilization <= 0 || cost <= 0 {
			return nil
		}
		if utilization < 100 {
			base := cost * 100 / utilization
			if !validSharedUsageNumber(base) {
				return nil
			}
			credits := 0.0
			return &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateSource: "dynamic"}
		}
		state.FrozenBase = &cost
		state.Source = "observed"
	}
	base := *state.FrozenBase
	credits := math.Max(0, cost-base)
	return &SubscriptionCostEstimate{EstimatedTotalCost: &base, CreditsUsedCost: &credits, EstimateFrozen: true, EstimateSource: state.Source}
}

type SubscriptionCostSnapshot struct {
	Observation CarpoolResetObservation
	Slot        time.Time
	Estimate    *SubscriptionCostEstimate
}

// EstimateForWindow only combines a live window with a snapshot for that same cycle.
func (s *SubscriptionCostSnapshot) EstimateForWindow(reset time.Time) *SubscriptionCostEstimate {
	if s == nil || reset.IsZero() || reset.Sub(s.Observation.ResetAt).Abs() > time.Minute {
		return nil
	}
	return s.Estimate
}

type SubscriptionCostRepository interface {
	Sample(context.Context, int64, string, CarpoolResetObservation, time.Time) (*SubscriptionCostSnapshot, error)
	Read(context.Context, int64, time.Time) (*SubscriptionCostSnapshot, error)
	Candidates(context.Context, int64, int) ([]int64, error)
}

type SubscriptionCostService struct {
	repo SubscriptionCostRepository
}

func NewSubscriptionCostService(repo SubscriptionCostRepository) *SubscriptionCostService {
	return &SubscriptionCostService{repo: repo}
}

// SubscriptionCostSampleInterval defines the sampling slot shared by the scheduler and repository.
const SubscriptionCostSampleInterval = 10 * time.Minute

type subscriptionCostSlotKey struct{}

func withSubscriptionCostSlot(ctx context.Context, slot time.Time) context.Context {
	return context.WithValue(ctx, subscriptionCostSlotKey{}, slot)
}

func subscriptionCostSlot(ctx context.Context) time.Time {
	slot, _ := ctx.Value(subscriptionCostSlotKey{}).(time.Time)
	return slot
}

func (s *SubscriptionCostService) sample(ctx context.Context, accountID int64, identity string, usage *OpenAIQuotaUsage, slot time.Time) (*SubscriptionCostSnapshot, error) {
	if s == nil || slot.IsZero() {
		return nil, nil
	}
	identity = strings.TrimSpace(identity)
	now := time.Now()
	observation, ok := ReadCarpoolResetObservation(usage, identity, now)
	if !ok {
		return nil, nil
	}
	if !usage.costObservedAt.IsZero() {
		observation.ObservedAt = usage.costObservedAt
	}
	if observation.ObservedAt.Before(slot) || !observation.ObservedAt.Before(slot.Add(SubscriptionCostSampleInterval)) {
		return nil, nil
	}
	return s.repo.Sample(ctx, accountID, identity, observation, slot)
}

func (s *SubscriptionCostService) read(ctx context.Context, accountID int64, now time.Time) (*SubscriptionCostSnapshot, error) {
	if s == nil {
		return nil, nil
	}
	return s.repo.Read(ctx, accountID, now)
}

func (s *OpenAIQuotaService) readSubscriptionCost(ctx context.Context, accountID int64, now time.Time) (*SubscriptionCostSnapshot, error) {
	return s.subscriptionCost.read(ctx, accountID, now)
}
