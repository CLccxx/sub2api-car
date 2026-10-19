package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCarpoolSettingsAreAdminOnly(t *testing.T) {
	now := time.Now()
	g := &service.Group{ID: 1, CarpoolMembers: 4, CarpoolQuotaSyncedAt: &now}
	admin := GroupFromServiceAdmin(g)
	require.Equal(t, 4, admin.CarpoolMembers)
	require.Equal(t, &now, admin.CarpoolQuotaSyncedAt)
	data, err := json.Marshal(GroupFromService(g))
	require.NoError(t, err)
	require.NotContains(t, string(data), "carpool_members")
	require.NotContains(t, string(data), "carpool_quota_synced_at")
}
