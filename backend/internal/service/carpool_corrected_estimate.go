package service

import (
	"math"

	"github.com/shopspring/decimal"
)

// carpoolCorrectedEstimate 把账号额度矫正叠加到原始订阅预估上。
// 返回 false 表示结果无效（原始预估缺失或非法、矫正后不大于 0 或超出有效范围），
// 调用方应保留现有周限额，不展示虚构的矫正估算。
func carpoolCorrectedEstimate(rawEstimate, correction float64) (decimal.Decimal, bool) {
	if math.IsNaN(rawEstimate) || math.IsInf(rawEstimate, 0) || rawEstimate <= 0 || rawEstimate > 1e10 ||
		math.IsNaN(correction) || math.IsInf(correction, 0) {
		return decimal.Zero, false
	}
	corrected := decimal.NewFromFloat(rawEstimate).Add(decimal.NewFromFloat(correction))
	if corrected.Sign() <= 0 || corrected.GreaterThan(decimal.NewFromInt(10_000_000_000)) {
		return decimal.Zero, false
	}
	return corrected, true
}
