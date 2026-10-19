package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type resetReceiptSpy struct {
	calls     int
	accountID int64
	identity  string
	request   string
	windows   int
	err       error
}

func (r *resetReceiptSpy) RecordResetReceipt(_ context.Context, id int64, identity, request string, _ time.Time, windows int) error {
	r.calls++
	r.accountID = id
	r.identity = identity
	r.request = request
	r.windows = windows
	return r.err
}

func TestCarpoolResetSuccessfulConsumeReceipt(t *testing.T) {
	for _, scenario := range []string{"ok", "no_credit", "zero windows", "storage failure"} {
		t.Run(scenario, func(t *testing.T) {
			account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "shared"}}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{9: account}}
			cache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "POST", r.Method)
				w.Header().Set("Content-Type", "application/json")
				switch scenario {
				case "no_credit":
					_, _ = w.Write([]byte(`{"code":"no_credit","windows_reset":0}`))
				case "zero windows":
					_, _ = w.Write([]byte(`{"code":"ok","windows_reset":0}`))
				default:
					_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
				}
			}))
			defer server.Close()
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, cache, nil), newQuotaRedirectingFactory(server), nil)
			recorder := &resetReceiptSpy{}
			if scenario == "storage failure" {
				recorder.err = errors.New("database unavailable")
			}
			svc.resetReceiptRecorder = recorder
			result, err := svc.ResetCreditTargeted(context.Background(), 9, "secret-credit", "stable-request")
			require.NoError(t, err, "receipt failure must not encourage another consume")
			require.NotNil(t, result)
			require.Equal(t, 1, calls)
			if scenario == "ok" || scenario == "storage failure" {
				require.Equal(t, 1, recorder.calls)
				require.Equal(t, CarpoolResetIdentityHash("shared"), recorder.identity)
				require.Equal(t, CarpoolResetIdentityHash("stable-request"), recorder.request)
				require.Equal(t, 2, recorder.windows)
			} else {
				require.Zero(t, recorder.calls)
			}
		})
	}
}
