package service

import "context"

// QuotaResetCoordinator serializes consumption per account and durably retains
// unresolved attempts. execute must use the supplied credit and request identity;
// a generic request can resume an older generic attempt with its original ID.
type QuotaResetCoordinator interface {
	Run(ctx context.Context, accountID int64, creditID, requestID string, execute func(context.Context, string, string) (*OpenAIQuotaResetResult, error)) (*OpenAIQuotaResetResult, error)
}

type quotaResetFirstDispatchKey struct{}

// WithQuotaResetFirstDispatch marks the first execution of a durable consume
// intent while the coordinator still holds the account's consumption lock.
func WithQuotaResetFirstDispatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, quotaResetFirstDispatchKey{}, true)
}
