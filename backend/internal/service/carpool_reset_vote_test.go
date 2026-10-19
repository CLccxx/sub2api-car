package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type voteRepoStub struct {
	CarpoolResetVoteRepository
	p                                          CarpoolResetVote
	dispatches, successes, awaits, completions int
	allow                                      bool
}

func (r *voteRepoStub) Load(context.Context, int64) (*CarpoolResetVote, error) {
	p := r.p
	return &p, nil
}
func (r *voteRepoStub) Dispatch(context.Context, int64) (bool, error) {
	r.dispatches++
	return r.allow, nil
}
func (r *voteRepoStub) AwaitConfirmation(context.Context, int64) error { r.awaits++; return nil }
func (r *voteRepoStub) Succeed(context.Context, int64, *OpenAIQuotaResetResult) (int64, error) {
	r.successes++
	r.p.Status = "applying"
	r.p.EventID = 7
	return 7, nil
}
func (r *voteRepoStub) Complete(context.Context, int64) error { r.completions++; return nil }

type voteCoordinatorStub struct {
	QuotaResetCoordinator
	saved *OpenAIQuotaResetResult
	pairs []string
}

type blockingQuotaCoordinator struct {
	calls  int
	credit string
}

func (c *blockingQuotaCoordinator) Run(_ context.Context, _ int64, credit, request string, _ func(context.Context, string, string) (*OpenAIQuotaResetResult, error)) (*OpenAIQuotaResetResult, error) {
	c.calls++
	c.credit = credit
	if request == "" {
		return nil, errors.New("request identity missing")
	}
	return nil, errors.New("blocked without network")
}

func TestCarpoolVoteAllConsumptionEntrypointsUseCoordinator(t *testing.T) {
	c := &blockingQuotaCoordinator{}
	s := &OpenAIQuotaService{resetCoordinator: c}
	_, err := s.ResetCredit(context.Background(), 9)
	require.EqualError(t, err, "blocked without network")
	_, err = s.ResetCreditTargeted(context.Background(), 9, "one-card", "request")
	require.EqualError(t, err, "blocked without network")
	require.Equal(t, 2, c.calls)
	require.Equal(t, "one-card", c.credit)
}

func (c *voteCoordinatorStub) Run(ctx context.Context, a int64, card, key string, fn func(context.Context, string, string) (*OpenAIQuotaResetResult, error)) (*OpenAIQuotaResetResult, error) {
	c.pairs = append(c.pairs, card+":"+key)
	if c.saved != nil {
		return c.saved, nil
	}
	v, err := fn(ctx, card, key)
	if err == nil && v.Code == "ok" {
		c.saved = v
	}
	return v, err
}

func TestCarpoolVoteUnknownRetryKeepsCardAndRequest(t *testing.T) {
	r := &voteRepoStub{p: CarpoolResetVote{ID: 1, Status: "awaiting_confirmation", Dispatched: true, CreditID: "first-card", RequestID: "fixed", Eligibility: CarpoolVoteEligibility{Source: &CarpoolResetSource{AccountID: 9}}}, allow: true}
	c := &voteCoordinatorStub{}
	calls := 0
	s := &CarpoolResetVoteService{repo: r, coordinator: c, consume: func(context.Context, int64, string, string) (*OpenAIQuotaResetResult, error) {
		calls++
		return nil, errors.New("timeout")
	}}
	s.process(context.Background(), 1)
	s.process(context.Background(), 1)
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"first-card:fixed", "first-card:fixed"}, c.pairs)
	require.Equal(t, 2, r.awaits)
	require.Zero(t, r.successes)
}

func TestCarpoolVoteKnownSuccessNeverConsumesAgain(t *testing.T) {
	r := &voteRepoStub{p: CarpoolResetVote{ID: 1, Status: "awaiting_confirmation", Dispatched: true, CreditID: "card", RequestID: "fixed", Eligibility: CarpoolVoteEligibility{Source: &CarpoolResetSource{AccountID: 9}}}, allow: true}
	c := &voteCoordinatorStub{saved: &OpenAIQuotaResetResult{Code: "ok", WindowsReset: 1}}
	s := &CarpoolResetVoteService{repo: r, coordinator: c, consume: func(context.Context, int64, string, string) (*OpenAIQuotaResetResult, error) {
		t.Fatal("known success must not dispatch")
		return nil, nil
	}}
	s.process(context.Background(), 1)
	require.Equal(t, 1, r.successes)
	require.Zero(t, r.dispatches)
}
