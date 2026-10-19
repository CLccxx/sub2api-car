package service

import (
	"context"
	"errors"
	"log"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

const MaxBulkSubscriptionActions = 100

const MaxGuardedSubscriptionResetLifetime = 10 * time.Minute
const GuardedSubscriptionResetReplayTTL = time.Hour

var errBulkSubscriptionInactive = errors.New("subscription is no longer active")

// BulkSubscriptionActionInput applies one operation to a bounded set of subscriptions.
type BulkSubscriptionActionInput struct {
	SubscriptionIDs []int64    `json:"subscription_ids"`
	Action          string     `json:"action"`
	Days            int        `json:"days,omitempty"`
	Daily           bool       `json:"daily,omitempty"`
	Weekly          bool       `json:"weekly,omitempty"`
	Monthly         bool       `json:"monthly,omitempty"`
	ActiveOnly      bool       `json:"active_only,omitempty"`
	ExecuteBefore   *time.Time `json:"execute_before,omitempty"`
}

// Validate checks the entire request before any subscription is changed.
func (input *BulkSubscriptionActionInput) Validate() error {
	if input == nil {
		return ErrSubscriptionNilInput
	}
	if len(input.SubscriptionIDs) == 0 || len(input.SubscriptionIDs) > MaxBulkSubscriptionActions {
		return infraerrors.BadRequest("INVALID_BULK_SUBSCRIPTIONS", "subscription_ids must contain between 1 and 100 IDs")
	}
	for _, id := range input.SubscriptionIDs {
		if id <= 0 {
			return infraerrors.BadRequest("INVALID_SUBSCRIPTION_ID", "subscription IDs must be positive")
		}
	}
	if input.ActiveOnly || input.ExecuteBefore != nil {
		now := time.Now()
		if input.Action != "reset_quota" || !input.ActiveOnly || input.ExecuteBefore == nil ||
			!input.ExecuteBefore.After(now) || input.ExecuteBefore.After(now.Add(MaxGuardedSubscriptionResetLifetime)) {
			return infraerrors.BadRequest("INVALID_RESET_DEADLINE", "guarded reset requires an execution deadline within the next 10 minutes")
		}
	}
	switch input.Action {
	case "extend":
		if input.Days == 0 || input.Days < -MaxValidityDays || input.Days > MaxValidityDays {
			return infraerrors.BadRequest("INVALID_ADJUSTMENT_DAYS", "days must be nonzero and between -36500 and 36500")
		}
	case "reset_quota":
		if !input.Daily && !input.Weekly && !input.Monthly {
			return ErrInvalidInput
		}
	case "revoke", "restore":
	default:
		return infraerrors.BadRequest("INVALID_SUBSCRIPTION_ACTION", "action must be extend, reset_quota, revoke, or restore")
	}
	return nil
}

type BulkSubscriptionActionItemResult struct {
	SubscriptionID int64  `json:"subscription_id"`
	Success        bool   `json:"success"`
	Error          string `json:"error,omitempty"`
	Skipped        bool   `json:"skipped,omitempty"`
}

type BulkSubscriptionActionResult struct {
	SuccessCount int                                `json:"success_count"`
	FailedCount  int                                `json:"failed_count"`
	SkippedCount int                                `json:"skipped_count,omitempty"`
	Results      []BulkSubscriptionActionItemResult `json:"results"`
}

// BulkSubscriptionAction preserves input order and executes duplicate IDs only once.
// Individual failures remain in the result instead of returning a top-level error,
// allowing callers to retain the completed portion of a partially successful batch.
func (s *SubscriptionService) BulkSubscriptionAction(ctx context.Context, input *BulkSubscriptionActionInput) (*BulkSubscriptionActionResult, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	result := &BulkSubscriptionActionResult{
		Results: make([]BulkSubscriptionActionItemResult, 0, len(input.SubscriptionIDs)),
	}
	seen := make(map[int64]struct{}, len(input.SubscriptionIDs))
	for _, id := range input.SubscriptionIDs {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		err := ctx.Err()
		if err == nil {
			var changed *UserSubscription
			// Single-item services may read again or update status after their first
			// write. Keep all those steps in one transaction, so a failed item can
			// be retried without repeating a previously committed extension/reset.
			err = s.withSubscriptionUpdateTx(ctx, func(txCtx context.Context) error {
				var mutationErr error
				switch input.Action {
				case "extend":
					changed, mutationErr = s.ExtendSubscription(txCtx, id, input.Days)
				case "reset_quota":
					if input.ActiveOnly {
						if !input.ExecuteBefore.After(s.now()) {
							return infraerrors.BadRequest("RESET_DEADLINE_EXCEEDED", "reset execution deadline has passed")
						}
						sub, readErr := s.userSubRepo.GetByIDForUpdate(txCtx, id)
						if errors.Is(readErr, ErrSubscriptionNotFound) {
							return errBulkSubscriptionInactive
						}
						if readErr != nil {
							return readErr
						}
						now := s.now()
						if !input.ExecuteBefore.After(now) {
							return infraerrors.BadRequest("RESET_DEADLINE_EXCEEDED", "reset execution deadline has passed")
						}
						if sub.DeletedAt != nil || sub.Status != SubscriptionStatusActive || !sub.ExpiresAt.After(now) {
							return errBulkSubscriptionInactive
						}
					}
					changed, mutationErr = s.AdminResetQuota(txCtx, id, input.Daily, input.Weekly, input.Monthly)
				case "revoke":
					changed, mutationErr = s.userSubRepo.GetByID(txCtx, id)
					if mutationErr == nil {
						mutationErr = s.RevokeSubscription(txCtx, id)
					}
				case "restore":
					changed, mutationErr = s.RestoreSubscription(txCtx, id)
				}
				return mutationErr
			})
			if err == nil && changed != nil {
				// Invalidate again after commit: concurrent readers could have filled
				// a cache with the old row while the transaction was still open.
				if cacheErr := s.invalidateSubscriptionCaches(changed.UserID, changed.GroupID); cacheErr != nil {
					log.Printf("[SubscriptionBulkAction] committed action=%s subscription_id=%d cache_error=%s", input.Action, id, logredact.RedactText(cacheErr.Error()))
				}
			}
		}
		item := BulkSubscriptionActionItemResult{SubscriptionID: id, Success: err == nil}
		if errors.Is(err, errBulkSubscriptionInactive) {
			result.SkippedCount++
			item.Skipped = true
		} else if err != nil {
			result.FailedCount++
			item.Error = infraerrors.Message(err)
			if errors.Is(err, context.Canceled) {
				item.Error = context.Canceled.Error()
			} else if errors.Is(err, context.DeadlineExceeded) {
				item.Error = context.DeadlineExceeded.Error()
			} else if infraerrors.Code(err) >= 500 {
				log.Printf("[SubscriptionBulkAction] action=%s subscription_id=%d error=%s", input.Action, id, logredact.RedactText(err.Error()))
			}
		} else {
			result.SuccessCount++
		}
		result.Results = append(result.Results, item)
	}
	return result, nil
}
