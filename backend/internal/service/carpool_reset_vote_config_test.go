package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminUpdateAccountExtraPreservesResetVoteMinimum(t *testing.T) {
	const accountID = 9
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{ResetVoteMin7DPercentExtraKey: 80.0},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	err := svc.UpdateAccountExtra(context.Background(), accountID, map[string]any{
		ResetVoteMin7DPercentExtraKey: 0.0,
		"custom":                      true,
	})
	require.NoError(t, err)
	require.Equal(t, 80.0, repo.accounts[accountID].Extra[ResetVoteMin7DPercentExtraKey])
	require.Equal(t, true, repo.accounts[accountID].Extra["custom"])
}
