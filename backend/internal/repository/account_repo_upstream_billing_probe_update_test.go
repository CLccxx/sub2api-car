package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// Keep the mock row layout in the same order as the locked SELECT and Scan.
func accountProbeMergeMockColumns() []string {
	return []string{
		"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged",
		"enabled", "rate_sync_enabled", "snapshot",
		"ollama_session", "ollama_auto", "ollama_snapshot",
		"opencode_group_unchanged", "opencode_auto", "opencode_snapshot",
		"grok_observed_models", "upstream_model_metadata", "group_sync_enabled",
		"is_shadow", "carpool_quota_correction_usd",
	}
}

func TestLockAndMergeAccountProtectsCarpoolQuotaCorrection(t *testing.T) {
	zero, explicit := 0.0, -25.75
	for _, tt := range []struct {
		name          string
		accountType   string
		currentShadow bool
		current       []byte
		explicit      *float64
		want          *float64
		wantErr       error
	}{
		{"locked row wins over stale extra", service.AccountTypeOAuth, false, []byte(`-25.75`), nil, &explicit, nil},
		{"deleted correction stays deleted", service.AccountTypeOAuth, false, nil, nil, nil, nil},
		{"ineligible result drops correction", service.AccountTypeAPIKey, false, []byte(`-25.75`), nil, nil, nil},
		{"explicit value wins", service.AccountTypeOAuth, false, []byte(`10`), &explicit, &explicit, nil},
		{"explicit zero removes key", service.AccountTypeOAuth, false, []byte(`10`), &zero, nil, nil},
		{"locked shadow rejects explicit intent", service.AccountTypeOAuth, true, nil, &explicit, nil, service.ErrCarpoolQuotaCorrectionAccountInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			mock.ExpectQuery(`(?s)SELECT.*extra -> '`+service.CarpoolQuotaCorrectionExtraKey+`'.*FOR NO KEY UPDATE`).
				WithArgs(int64(27), service.PlatformOpenAI, tt.accountType, "{}", nil).
				WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
					AddRow(false, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, tt.currentShadow, tt.current))
			account := &service.Account{
				ID: 27, Platform: service.PlatformOpenAI, Type: tt.accountType,
				Extra: map[string]any{service.CarpoolQuotaCorrectionExtraKey: 99.0, "note": "keep"},
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, service.AccountBillingSettingsUpdate{}, tt.explicit)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, "keep", got["note"])
				if tt.want == nil {
					require.NotContains(t, got, service.CarpoolQuotaCorrectionExtraKey)
				} else {
					require.Equal(t, *tt.want, got[service.CarpoolQuotaCorrectionExtraKey])
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRepositoryRawAndBulkUpdatesIgnoreCarpoolQuotaCorrection(t *testing.T) {
	ctx := context.Background()
	client, mock := newOllamaCloudUsageRepositoryTestClient(t)
	input := map[string]any{service.CarpoolQuotaCorrectionExtraKey: 99.0, "codex_usage_updated_at": "keep"}
	rawRepo := newAccountRepositoryWithSQL(client, nil, nil)
	mock.ExpectExec(`UPDATE accounts SET extra =`).
		WithArgs(`{"codex_usage_updated_at":"keep"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, rawRepo.UpdateExtra(ctx, 27, input))
	require.NoError(t, mock.ExpectationsWereMet())

	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	bulkRepo := newAccountRepositoryWithSQL(nil, exec, nil)
	_, err := bulkRepo.BulkUpdate(ctx, []int64{27}, service.AccountBulkUpdate{Extra: input})
	require.NoError(t, err)
	require.NotEmpty(t, exec.execArgs)
	bulkExtra, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok, "bulk extra must serialize as JSON bytes")
	require.JSONEq(t, `{"codex_usage_updated_at":"keep"}`, string(bulkExtra))
	require.Contains(t, input, service.CarpoolQuotaCorrectionExtraKey)
}

func TestLockAndMergeAccountProbeExtraUsesCurrentDatabaseSnapshot(t *testing.T) {
	tests := []struct {
		name              string
		identityUnchanged bool
		databaseEnabled   any
		databaseSnapshot  any
		inputExtra        map[string]any
		wantSnapshot      any
		wantEnabled       any
	}{
		{
			name:              "ordinary edit preserves current enable flag and snapshot created after account load",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra:        map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
			wantSnapshot:      map[string]any{"status": "ok"},
			wantEnabled:       true,
		},
		{
			name:              "identity change clears stale snapshot",
			identityUnchanged: false,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
		{
			name:              "current explicit disable clears snapshot",
			identityUnchanged: true,
			databaseEnabled:   []byte(`false`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: false,
		},
		{
			name:              "missing database snapshot is not resurrected from stale input",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  nil,
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
				WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
					AddRow(tt.identityUnchanged, false, true, tt.databaseEnabled, nil, tt.databaseSnapshot, nil, nil, nil, false, nil, nil, nil, nil, nil, false, nil))

			account := &service.Account{
				ID:          27,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       tt.inputExtra,
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, service.AccountBillingSettingsUpdate{}, nil)
			require.NoError(t, err)
			if tt.wantSnapshot == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeExtraKey)
			} else {
				require.Equal(t, tt.wantSnapshot, got[service.UpstreamBillingProbeExtraKey])
			}
			require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func probeBoolPtr(value bool) *bool {
	return &value
}

// The probe switch drives the rate-sync switch and never the other way round:
// syncing depends on probing, so a row where sync is on but the probe key is
// missing must lose the sync flag instead of silently gaining periodic
// outbound calls on the next unrelated edit.
func TestLockAndMergeAccountProbeExtraNeverInfersProbeFromRateSync(t *testing.T) {
	tests := []struct {
		name                 string
		databaseEnabled      any
		databaseRateSync     any
		explicitProbeEnabled *bool
		explicitRateSync     *bool
		wantEnabled          any
		wantRateSync         any
	}{
		{
			name:             "sync on with missing probe key zeroes sync and keeps probing off",
			databaseEnabled:  nil,
			databaseRateSync: []byte(`true`),
			wantEnabled:      nil,
			wantRateSync:     false,
		},
		{
			name:             "sync on with probe off zeroes sync",
			databaseEnabled:  []byte(`false`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      false,
			wantRateSync:     false,
		},
		{
			name:             "both on in database stay on",
			databaseEnabled:  []byte(`true`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      true,
			wantRateSync:     true,
		},
		{
			name:                 "admin enabling both explicitly still turns probing on",
			databaseEnabled:      nil,
			databaseRateSync:     nil,
			explicitProbeEnabled: probeBoolPtr(true),
			explicitRateSync:     probeBoolPtr(true),
			wantEnabled:          true,
			wantRateSync:         true,
		},
		{
			name:                 "explicit probe disable clears sync",
			databaseEnabled:      []byte(`true`),
			databaseRateSync:     []byte(`true`),
			explicitProbeEnabled: probeBoolPtr(false),
			wantEnabled:          false,
			wantRateSync:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
				WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
					AddRow(true, false, true, tt.databaseEnabled, tt.databaseRateSync, nil, nil, nil, nil, false, nil, nil, nil, nil, nil, false, nil))

			account := &service.Account{
				ID:          31,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
			}
			got, err := lockAndMergeAccountProbeExtra(
				context.Background(), client, account, service.AccountBillingSettingsUpdate{ProbeEnabled: tt.explicitProbeEnabled, RateSyncEnabled: tt.explicitRateSync}, nil,
			)
			require.NoError(t, err)
			if tt.wantEnabled == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			}
			if tt.wantRateSync == nil {
				require.NotContains(t, got, service.UpstreamBillingRateSyncEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantRateSync, got[service.UpstreamBillingRateSyncEnabledExtraKey])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLockAndMergeAccountProbeExtraProtectsOllamaManagedFields(t *testing.T) {
	for _, identityUnchanged := range []bool{true, false} {
		t.Run(map[bool]string{true: "same identity keeps snapshot", false: "changed identity clears snapshot"}[identityUnchanged], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(29), service.PlatformAnthropic, service.AccountTypeAPIKey, `{"api_key":"key","base_url":"https://ollama.com"}`, nil).
				WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
					AddRow(identityUnchanged, identityUnchanged, true, nil, nil, nil, []byte(`"local-ciphertext"`), []byte(`true`), []byte(`{"status":"ok"}`), false, nil, nil, nil, nil, nil, false, nil))

			account := &service.Account{
				ID: 29, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "key", "base_url": "https://ollama.com"},
				Extra: map[string]any{
					service.OllamaCloudUsageSessionExtraKey:     "forged-ciphertext",
					service.OllamaCloudUsageAutoRefreshExtraKey: false,
					service.OllamaCloudUsageSnapshotExtraKey:    map[string]any{"status": "forged"},
				},
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, service.AccountBillingSettingsUpdate{}, nil)
			require.NoError(t, err)
			if identityUnchanged {
				require.Equal(t, "local-ciphertext", got[service.OllamaCloudUsageSessionExtraKey])
				require.Equal(t, true, got[service.OllamaCloudUsageAutoRefreshExtraKey])
				require.Equal(t, map[string]any{"status": "ok"}, got[service.OllamaCloudUsageSnapshotExtraKey])
			} else {
				require.NotContains(t, got, service.OllamaCloudUsageSessionExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageAutoRefreshExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageSnapshotExtraKey)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateExtraExplicitProbeDisableRemovesSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe":null}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeExtraKey: nil})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeExtraKey: nil},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
}

func TestBulkUpdateDisablingProbeRemovesSnapshot(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
	payload, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok)
	require.Equal(t, `{"upstream_billing_probe_enabled":false}`, string(payload))
}

func TestBulkUpdateProbeEligibilityMismatchRollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	enabled := true
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* WHERE id = ANY\(\$2\) AND deleted_at IS NULL AND type = \$3`).
		WithArgs(sqlmock.AnyArg(), `{27,28}`, service.AccountTypeAPIKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		ProbeEnabled: &enabled,
	})

	require.ErrorIs(t, err, service.ErrUpstreamBillingProbeAccountInvalid)
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsAtomicallyClearsProbeForOpenAIAPIKeyIdentityChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials IS DISTINCT FROM \$1::jsonb.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountBillingSettingsAndCarpoolQuotaCorrectionCoexist(t *testing.T) {
	const groupKey = service.UpstreamBillingGroupSyncEnabledExtraKey
	const correctionKey = service.CarpoolQuotaCorrectionExtraKey
	staleRate, staleNotes := 9.0, "stale notes"
	correction, zero := -25.75, 0.0
	disabled := false

	for _, tt := range []struct {
		name        string
		accountType string
		settings    service.AccountBillingSettingsUpdate
		correction  *float64
		wantExtra   map[string]any
	}{
		{
			name: "omitted group sync preserves locked flag and synced fields", accountType: service.AccountTypeAPIKey,
			wantExtra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true, groupKey: true, "local": "keep"},
		},
		{
			name: "explicit group sync disable beats the locked flag", accountType: service.AccountTypeAPIKey,
			settings:  service.AccountBillingSettingsUpdate{GroupSyncEnabled: &disabled},
			wantExtra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true, groupKey: false, "local": "keep"},
		},
		{
			name: "correction-only edit does not write stale name notes or rate", accountType: service.AccountTypeOAuth,
			correction: &correction,
			wantExtra:  map[string]any{correctionKey: -25.75, "local": "keep"},
		},
		{
			name: "clear correction with an explicit rename and notes clear", accountType: service.AccountTypeOAuth,
			settings:   service.AccountBillingSettingsUpdate{GroupSyncEnabled: &disabled, SetName: true, SetNotes: true},
			correction: &zero,
			wantExtra:  map[string]any{"local": "keep"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, mock := newOllamaCloudUsageRepositoryTestClient(t)
			client.Account.Use(func(next dbent.Mutator) dbent.Mutator {
				return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
					_, writesName := m.Field(dbaccount.FieldName)
					require.Equal(t, tt.settings.SetName, writesName)
					require.Equal(t, tt.settings.SetNotes, m.FieldCleared(dbaccount.FieldNotes))
					_, writesNotes := m.Field(dbaccount.FieldNotes)
					require.False(t, writesNotes, "never persist stale notes")
					_, writesRate := m.Field(dbaccount.FieldRateMultiplier)
					require.False(t, writesRate, "never persist a stale rate")
					extra, ok := m.Field(dbaccount.FieldExtra)
					require.True(t, ok)
					require.Equal(t, tt.wantExtra, extra)
					return next.Mutate(ctx, m)
				})
			})

			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*extra -> '`+groupKey+`'.*extra -> '`+correctionKey+`'.*FOR NO KEY UPDATE`).
				WithArgs(int64(27), service.PlatformOpenAI, tt.accountType, "{}", nil).
				WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
					AddRow(false, false, true, []byte(`true`), nil, nil, nil, nil, nil, false, nil, nil, nil, nil, []byte(`true`), false, []byte(`10`)))
			mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
				WithArgs(int64(27)).WillReturnRows(updatedAccountRows(27, `{}`))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			repo := newAccountRepositoryWithSQL(client, nil, nil)
			account := &service.Account{
				ID: 27, Name: "form name", Notes: &staleNotes, RateMultiplier: &staleRate,
				Platform: service.PlatformOpenAI, Type: tt.accountType, Status: service.StatusActive,
				Extra: map[string]any{groupKey: false, correctionKey: 99.0, "local": "keep"},
			}
			if tt.settings.SetNotes {
				account.Notes = nil
			}
			if tt.correction == nil {
				require.NoError(t, repo.UpdateWithAccountBillingSettings(context.Background(), account, tt.settings))
			} else {
				require.NoError(t, repo.UpdateWithCarpoolQuotaCorrection(context.Background(), account, tt.settings, *tt.correction))
			}
			require.Equal(t, tt.wantExtra, account.Extra)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateWithAccountBillingSettingsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
		WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
		WillReturnRows(sqlmock.NewRows(accountProbeMergeMockColumns()).
			AddRow(true, false, true, []byte(`true`), []byte(`true`), []byte(`{"status":"ok"}`), nil, nil, nil, false, nil, nil, nil, nil, nil, false, nil))
	mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
		WithArgs(int64(27)).
		WillReturnRows(updatedAccountRows(27, `{"upstream_billing_probe_enabled":false,"upstream_billing_rate_sync_enabled":false}`))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	account := &service.Account{
		ID:          27,
		Name:        "test",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra: map[string]any{
			service.UpstreamBillingProbeExtraKey: map[string]any{"status": "stale"},
		},
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
	}

	probeDisabled := false
	err = repo.UpdateWithAccountBillingSettings(context.Background(), account, service.AccountBillingSettingsUpdate{ProbeEnabled: &probeDisabled})

	require.EqualError(t, err, "outbox failed")
	require.Equal(t, false, account.Extra[service.UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, false, account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey])
	require.NotContains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials IS DISTINCT FROM \$1::jsonb.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	name := "renamed"
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET name = \$1.*WHERE id = ANY\(\$2\)`).
		WithArgs(name, `{27,28}`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{Name: &name})

	require.EqualError(t, err, "outbox failed")
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func updatedAccountRows(id int64, extra string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(dbaccount.Columns).AddRow(
		id, now, now, nil, "test", nil, service.PlatformOpenAI, service.AccountTypeAPIKey,
		[]byte(`{"api_key":"sk-test"}`), []byte(extra), nil, nil, 1, nil, 1, 1.0,
		service.StatusActive, nil, nil, nil, false, true, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, service.QuotaDimensionGlobal,
	)
}
