package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCarpoolQuotaCorrectionUSD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float64
		valid bool
	}{
		{"zero", 0, true},
		{"positive", 100, true},
		{"negative", -100, true},
		{"two decimals", 12.34, true},
		{"three decimals", 12.345, false},
		{"positive overflow", 10_000_000_000.01, false},
		{"negative overflow", -10_000_000_000.01, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCarpoolQuotaCorrectionUSD(tc.value)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCarpoolQuotaCorrectionUSD(t *testing.T) {
	require.Equal(t, 0.0, CarpoolQuotaCorrectionUSD(nil))
	require.Equal(t, 0.0, CarpoolQuotaCorrectionUSD(map[string]any{}))
	require.Equal(t, 100.0, CarpoolQuotaCorrectionUSD(map[string]any{CarpoolQuotaCorrectionExtraKey: 100.0}))
	require.Equal(t, -50.5, CarpoolQuotaCorrectionUSD(map[string]any{CarpoolQuotaCorrectionExtraKey: -50.5}))
	// 非法存储兜底为 0，读取方永远拿到有限数值。
	require.Equal(t, 0.0, CarpoolQuotaCorrectionUSD(map[string]any{CarpoolQuotaCorrectionExtraKey: "100"}))
}
