package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.CarpoolSubscriptionReader = (*userSubscriptionRepository)(nil)

func (r *userSubscriptionRepository) GetActiveCarpoolSubscription(ctx context.Context, userID, groupID int64) (*service.UserSubscription, bool, error) {
	client := clientFromContext(ctx, r.client)
	carpool, err := client.Group.Query().Where(
		group.IDEQ(groupID),
		group.PlatformEQ(service.PlatformOpenAI),
		group.SubscriptionTypeEQ(service.SubscriptionTypeSubscription),
		group.CarpoolMembersGTE(2),
		group.CarpoolMembersLTE(5),
	).Exist(ctx)
	if err != nil || !carpool {
		return nil, carpool, err
	}
	sub, err := r.GetActiveByUserIDAndGroupID(ctx, userID, groupID)
	return sub, true, err
}
