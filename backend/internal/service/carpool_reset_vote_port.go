package service

import (
	"context"
	"time"
)

// Only this projection crosses the user API boundary.
type ResetVoteView struct {
	RequestResolved bool                   `json:"request_resolved,omitempty"`
	RequiredCount   int                    `json:"required_count"`
	MemberCount     int                    `json:"member_count"`
	CanCreate       bool                   `json:"can_create"`
	BlockedReason   *string                `json:"blocked_reason"`
	Proposal        *ResetVoteProposalView `json:"proposal"`
}

type ResetVoteProposalView struct {
	ID            int64     `json:"id"`
	Status        string    `json:"status"`
	AgreedCount   int       `json:"agreed_count"`
	RequiredCount int       `json:"required_count"`
	MyVote        bool      `json:"my_vote"`
	CanAgree      bool      `json:"can_agree"`
	ExpiresAt     time.Time `json:"expires_at"`
	Reason        *string   `json:"reason"`
}

type CarpoolVoteMember struct {
	UserID         int64
	SubscriptionID int64
	StartsAt       time.Time
}

type CarpoolVoteEligibility struct {
	Source             *CarpoolResetSource
	GroupID            int64
	RequiredCount      int
	Members            []CarpoolVoteMember
	MembershipRevision int64
	ReceiptCount       int64
	OperationCount     int64
	BlockedReason      string
}

type CarpoolResetVote struct {
	ID          int64
	Eligibility CarpoolVoteEligibility
	Observation CarpoolResetObservation
	Status      string
	ExpiresAt   time.Time
	CreditID    string
	RequestID   string
	Dispatched  bool
	EventID     int64
}

type CarpoolResetVoteRepository interface {
	Eligibility(context.Context, int64, int64) (*CarpoolVoteEligibility, error)
	Creation(context.Context, int64, int64, string) (*ResetVoteView, error)
	View(context.Context, int64, int64) (*ResetVoteView, error)
	Create(context.Context, int64, int64, string, CarpoolVoteEligibility, CarpoolResetObservation) (*ResetVoteView, error)
	Agree(context.Context, int64, int64, int64) (*ResetVoteView, error)
	Pending(context.Context) ([]int64, error)
	Load(context.Context, int64) (*CarpoolResetVote, error)
	Prepare(context.Context, int64, CarpoolResetObservation, string, string) (bool, error)
	Dispatch(context.Context, int64) (bool, error)
	AwaitConfirmation(context.Context, int64) error
	Fail(context.Context, int64, string) error
	Succeed(context.Context, int64, *OpenAIQuotaResetResult) (int64, error)
	Complete(context.Context, int64) error
}
