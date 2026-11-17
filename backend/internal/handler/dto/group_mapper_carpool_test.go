package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCarpoolSettingsAreAdminOnly(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bonus float64
	}{
		{name: "default", bonus: 100},
		{name: "custom", bonus: 125.75},
		{name: "zero", bonus: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			g := &service.Group{ID: 1, CarpoolMembers: 4, CarpoolBonusUSD: tt.bonus, CarpoolQuotaSyncedAt: &now}
			admin := GroupFromServiceAdmin(g)
			require.Equal(t, 4, admin.CarpoolMembers)
			require.Equal(t, tt.bonus, admin.CarpoolBonusUSD)
			require.Equal(t, &now, admin.CarpoolQuotaSyncedAt)

			data, err := json.Marshal(admin)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			require.Contains(t, fields, "carpool_bonus_usd", "zero must not be omitted")
			require.Equal(t, tt.bonus, fields["carpool_bonus_usd"])

			for _, user := range []*Group{GroupFromService(g), GroupFromServiceShallow(g)} {
				data, err := json.Marshal(user)
				require.NoError(t, err)
				require.NotContains(t, string(data), "carpool_members")
				require.NotContains(t, string(data), "carpool_bonus_usd")
				require.NotContains(t, string(data), "carpool_quota_synced_at")
			}
		})
	}
}
