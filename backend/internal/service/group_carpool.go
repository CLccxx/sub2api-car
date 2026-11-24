package service

import (
	"fmt"
	"math"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

const DefaultCarpoolBonusUSD = 100.0

// CarpoolConfig is an internal snapshot used to guard admin updates. The bonus
// has its own explicit-update guard; cron only changes the cap and timestamps.
type CarpoolConfig struct {
	Members          int
	Platform         string
	SubscriptionType string
	Status           string
}

func validCarpoolMembers(n int) bool { return n >= 2 && n <= 5 }

func validCarpoolBonus(bonus float64) bool {
	return !math.IsNaN(bonus) && !math.IsInf(bonus, 0) && bonus >= 0 && bonus <= 1e10 &&
		bonus == math.Round(bonus*100)/100
}

func validateCarpoolBonus(bonus float64) error {
	if !validCarpoolBonus(bonus) {
		return infraerrors.BadRequest("INVALID_CARPOOL_BONUS", "Carpool bonus must be between 0 and 10000000000 USD with at most two decimal places")
	}
	return nil
}

func applyCarpoolSettings(group *Group, previousLimit *float64) error {
	group.PreserveCarpoolWeeklyLimit = false
	group.AdjustCarpoolBonus = false
	if group.CarpoolMembers == 0 {
		return nil
	}
	if !validCarpoolMembers(group.CarpoolMembers) || group.Platform != PlatformOpenAI || !group.IsSubscriptionType() {
		return infraerrors.BadRequest("INVALID_CARPOOL_SETTINGS", "Carpool requires an OpenAI subscription group with 2 to 5 members")
	}
	if err := validateCarpoolBonus(group.CarpoolBonusUSD); err != nil {
		return err
	}
	group.Name = fmt.Sprintf("%d 人车分组", group.CarpoolMembers)
	group.Description = fmt.Sprintf("%d 人均分额度", group.CarpoolMembers)
	group.DailyLimitUSD, group.MonthlyLimitUSD = nil, nil
	group.PreserveCarpoolWeeklyLimit = previousLimit != nil && *previousLimit > 0
	if group.PreserveCarpoolWeeklyLimit {
		group.WeeklyLimitUSD = previousLimit
		if group.PreviousCarpoolBonusUSD != nil && *group.PreviousCarpoolBonusUSD != group.CarpoolBonusUSD {
			// The repository applies this delta to the latest cap, not this snapshot.
			delta := (math.Round(group.CarpoolBonusUSD*100) - math.Round(*group.PreviousCarpoolBonusUSD*100)) / 100
			limit := *previousLimit + delta
			if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
				return infraerrors.Conflict("CARPOOL_QUOTA_CHANGED", "Weekly quota cannot accommodate the bonus change; refresh the group and try again")
			}
			group.WeeklyLimitUSD = &limit
			group.AdjustCarpoolBonus = true
		}
	} else {
		limit, _ := carpoolWeeklyLimit(1600, group.CarpoolMembers, group.CarpoolBonusUSD)
		group.WeeklyLimitUSD = &limit
	}
	return nil
}

func carpoolWeeklyLimit(estimate float64, members int, bonusUSD float64) (float64, bool) {
	validated, ok := carpoolCorrectedEstimate(estimate, 0)
	if !ok {
		return 0, false
	}
	return carpoolWeeklyLimitFromEstimate(validated, members, bonusUSD)
}

func carpoolWeeklyLimitFromEstimate(estimate decimal.Decimal, members int, bonusUSD float64) (float64, bool) {
	if !validCarpoolMembers(members) || !validCarpoolBonus(bonusUSD) {
		return 0, false
	}
	// Split at full precision, truncating only the per-member share to cents.
	shareCents, _ := estimate.Shift(2).QuoRem(decimal.NewFromInt(int64(members)), 0)
	limit := float64(shareCents.IntPart()+int64(math.Round(bonusUSD*100))) / 100
	// Zero is treated as an unset cap; keep the existing cap instead.
	return limit, limit > 0
}
