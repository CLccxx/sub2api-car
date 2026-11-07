package service

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

type SubscriptionCostPendingUsage struct {
	AccountID   int64
	Identity    string
	StartedAt   time.Time
	ObservedAt  time.Time
	Observation *CarpoolResetObservation
}

type UsageAccountingScope struct {
	Account   *Account
	StartedAt time.Time
	Duration  time.Duration
	Headers   http.Header
}

func NewUsageAccountingScope(account *Account, result any, startedAt time.Time) UsageAccountingScope {
	scope := UsageAccountingScope{Account: account, StartedAt: startedAt}
	switch value := result.(type) {
	case *OpenAIForwardResult:
		if value != nil {
			scope.Headers = value.UpstreamHeaders
			scope.Duration = value.Duration
		}
	case *ForwardResult:
		if value != nil {
			scope.Headers = value.UpstreamHeaders
			scope.Duration = value.Duration
		}
	}
	return scope
}

type subscriptionCostPendingKey struct{}

func (s *OpenAIGatewayService) WrapSubscriptionCostAccountingTask(parent context.Context, task UsageRecordTask, scopes ...UsageAccountingScope) (UsageRecordTask, bool) {
	if s == nil {
		return task, false
	}
	return wrapSubscriptionCostAccountingTask(parent, task, s.subscriptionCost, scopes...)
}
func (s *GatewayService) WrapSubscriptionCostAccountingTask(parent context.Context, task UsageRecordTask, scopes ...UsageAccountingScope) (UsageRecordTask, bool) {
	if s == nil {
		return task, false
	}
	return wrapSubscriptionCostAccountingTask(parent, task, s.subscriptionCost, scopes...)
}

// Registration happens before queue submission. A failed registration forces the
// caller's synchronous fallback and leaves a negative marker on its usage row,
// keeping that cycle unavailable instead of freezing an incomplete aggregate.
func wrapSubscriptionCostAccountingTask(parent context.Context, task UsageRecordTask, observer *SubscriptionCostService, scopes ...UsageAccountingScope) (UsageRecordTask, bool) {
	if task == nil || observer == nil || observer.repo == nil || len(scopes) == 0 {
		return task, false
	}
	scope := scopes[0]
	account := scope.Account
	if account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return task, false
	}
	if parent == nil {
		parent = context.Background()
	}
	observed := time.Now()
	started := subscriptionCostAccountingTime(parent, account, scope.StartedAt, scope.Headers, observed)
	if scope.StartedAt.IsZero() && scope.Duration > 0 {
		started = observed.Add(-scope.Duration)
	}
	pending := SubscriptionCostPendingUsage{AccountID: account.ID, Identity: account.GetCredential("chatgpt_account_id"), StartedAt: started, ObservedAt: observed}
	if usage := subscriptionCostUsageFromHeaders(scope.Headers, pending.Identity, observed); usage != nil {
		if observation, ok := ReadCarpoolResetObservation(usage, pending.Identity, observed); ok {
			observation.ObservedAt = observed
			observation.CostEvidenceAt = started
			pending.Observation = &observation
		}
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	ticket, err := observer.repo.RegisterPending(ctx, pending)
	cancel()
	failed := err != nil
	if failed {
		ticket = -1
		slog.Warn("subscription_cost_registration_failed", "account_id", account.ID, "error", err)
	}
	return func(ctx context.Context) {
		ctx = WithUsageObservationTime(ctx, observed)
		ctx = context.WithValue(ctx, subscriptionCostPendingKey{}, ticket)
		// Keep exact dispatch attribution when a route has no pricing timestamp.
		ctx = context.WithValue(ctx, subscriptionCostStartKey{}, started)
		task(ctx)
	}, failed
}

type subscriptionCostStartKey struct{}

func (s *SubscriptionCostService) ensureAccounting(ctx context.Context, account *Account, result any, pricingAt time.Time) context.Context {
	if s == nil {
		return ctx
	}
	if _, ok := ctx.Value(subscriptionCostPendingKey{}).(int64); ok {
		return ctx
	}
	wrapped, _ := wrapSubscriptionCostAccountingTask(ctx, func(c context.Context) { ctx = c }, s, NewUsageAccountingScope(account, result, pricingAt))
	wrapped(ctx)
	return ctx
}
