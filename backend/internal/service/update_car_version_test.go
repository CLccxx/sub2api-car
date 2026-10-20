//go:build unit

package service

import "testing"

func TestCompareCustomReleaseVersions(t *testing.T) {
	tests := []struct {
		current, latest string
		want            int
	}{
		{"0.2.17-car.6", "0.2.17-car.7", -1},
		{"v0.2.17-car.9", "v0.2.17-car.10", -1},
		{"0.2.17-car.10", "0.2.17-car.9", 1},
		{"0.2.17-car.6", "0.2.17-car.6", 0},
		{"0.2.17-car.99", "0.2.18-car.1", -1},
		{"0.2.18-car.1", "0.2.17-car.99", 1},
		{"0.2.17", "0.2.18", -1},
	}
	for _, tt := range tests {
		t.Run(tt.current+"_to_"+tt.latest, func(t *testing.T) {
			if got := compareVersions(tt.current, tt.latest); got != tt.want {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}
