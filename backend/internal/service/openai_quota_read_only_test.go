package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryUsageReadOnlyNeverRecoversAgentIdentityTask(t *testing.T) {
	for _, taskID := range []string{"task-expired", ""} {
		t.Run(taskID, func(t *testing.T) {
			_, privateKey := newTestAgentIdentityKey(t)
			account := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
				"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": "runtime-test",
				"agent_private_key": privateKey, "task_id": taskID, "chatgpt_account_id": "account-test",
			}}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{20: account}}
			registerCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/task/register") {
					registerCalls++
					_, _ = w.Write([]byte(`{"task_id":"new-task"}`))
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_task_id"}}`))
			}))
			defer server.Close()
			oldBase := openAIAgentIdentityAuthAPIBaseURL
			openAIAgentIdentityAuthAPIBaseURL = server.URL
			t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldBase })
			invalidator := &agentIdentityWSInvalidationRecorder{}
			svc := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(server), nil)
			svc.agentIdentityWS = invalidator
			_, err := svc.QueryUsageReadOnly(context.Background(), 20)
			require.Error(t, err)
			require.Zero(t, registerCalls)
			require.Empty(t, invalidator.accountIDs)
			require.Equal(t, taskID, account.GetCredential("task_id"))
		})
	}
}
