package service

import (
	"fmt"
	"math"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func validCarpoolMembers(n int) bool { return n >= 2 && n <= 5 }

func applyCarpoolSettings(group *Group, previousLimit *float64) error {
	if group.CarpoolMembers == 0 {
		return nil
	}
	if !validCarpoolMembers(group.CarpoolMembers) || group.Platform != PlatformOpenAI || !group.IsSubscriptionType() {
		return infraerrors.BadRequest("INVALID_CARPOOL_SETTINGS", "Carpool requires an OpenAI subscription group with 2 to 5 members")
	}
	group.Name = fmt.Sprintf("%d 人车分组", group.CarpoolMembers)
	group.Description = fmt.Sprintf("%d 人均分额度", group.CarpoolMembers)
	group.DailyLimitUSD, group.MonthlyLimitUSD = nil, nil
	group.PreserveCarpoolWeeklyLimit = previousLimit != nil && *previousLimit > 0
	if group.PreserveCarpoolWeeklyLimit {
		group.WeeklyLimitUSD = previousLimit
	} else {
		limit := float64(160000/group.CarpoolMembers) / 100
		group.WeeklyLimitUSD = &limit
	}
	return nil
}

func carpoolWeeklyLimit(estimate float64, members int) (float64, bool) {
	if !validCarpoolMembers(members) || math.IsNaN(estimate) || math.IsInf(estimate, 0) || estimate < 100 || estimate > 1e10 {
		return 0, false
	}
	limit := math.Floor(math.Floor(estimate/100)*10000/float64(members)) / 100
	return limit, limit > 0
}
