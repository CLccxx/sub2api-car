package service

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCarpoolCorrectedEstimate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        float64
		correction float64
		expected   float64
		ok         bool
	}{
		{"no correction", 1600, 0, 1600, true},
		{"positive correction", 1600, 100, 1700, true},
		{"negative correction", 1600, -100, 1500, true},
		{"fractional", 1603, 0.5, 1603.5, true},
		{"zero raw", 0, 100, 0, false},
		{"negative raw", -100, 100, 0, false},
		{"NaN raw", math.NaN(), 100, 0, false},
		{"Inf raw", math.Inf(1), 100, 0, false},
		{"raw over limit", 1e10 + 1, 0, 0, false},
		{"correction to zero", 100, -100, 0, false},
		{"correction below zero", 100, -200, 0, false},
		{"corrected over limit", 1e10, 100, 0, false},
		{"NaN correction", 100, math.NaN(), 0, false},
		{"Inf correction", 100, math.Inf(1), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := carpoolCorrectedEstimate(tc.raw, tc.correction)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.expected, got.InexactFloat64())
			} else {
				require.True(t, got.IsZero())
			}
		})
	}
}
