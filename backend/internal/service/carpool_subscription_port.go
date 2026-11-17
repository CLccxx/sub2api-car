package service

import "context"

// CarpoolSubscriptionReader bypasses eventual subscription caches for groups
// whose current ledger can change their members' spendable quota immediately.
// Non-carpool groups keep the existing cache path.
type CarpoolSubscriptionReader interface {
	GetActiveCarpoolSubscription(context.Context, int64, int64) (*UserSubscription, bool, error)
}
