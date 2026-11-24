package service

import (
	"math"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var errInvalidCarpoolQuotaCorrection = infraerrors.BadRequest(
	"INVALID_CARPOOL_QUOTA_CORRECTION",
	"Quota correction must be a finite USD amount with at most two decimal places within ±10000000000",
)

// CarpoolQuotaCorrectionExtraKey 是账号级订阅额度矫正（USD）在 accounts.extra 中的键。
// 缺键视为 0；仅管理员单账号编辑可通过显式意图写入或清零。
const CarpoolQuotaCorrectionExtraKey = "carpool_quota_correction_usd"

// CarpoolQuotaCorrectionMaxAbsUSD 是允许的矫正幅度上限（与现有额度金额上限一致）。
const carpoolQuotaCorrectionMaxAbsUSD = 10_000_000_000

// ValidateCarpoolQuotaCorrectionUSD 校验有符号矫正金额：有限、最多两位小数、幅度不超上限。
func ValidateCarpoolQuotaCorrectionUSD(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > carpoolQuotaCorrectionMaxAbsUSD {
		return errInvalidCarpoolQuotaCorrection
	}
	if value != math.Round(value*100)/100 {
		return errInvalidCarpoolQuotaCorrection
	}
	return nil
}

// CarpoolQuotaCorrectionUSD 从 extra 读取矫正值，缺键或非法存储都按 0 处理。
// 存储侧写入前已校验；这里兜底保证读取方永远拿到有限数值。
func CarpoolQuotaCorrectionUSD(extra map[string]any) float64 {
	raw, ok := extra[CarpoolQuotaCorrectionExtraKey]
	if !ok || raw == nil {
		return 0
	}
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > carpoolQuotaCorrectionMaxAbsUSD {
		return 0
	}
	return value
}
