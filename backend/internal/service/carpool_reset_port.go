package service

import (
	"context"
	"time"
)

type CarpoolResetSource struct {
	GroupID           int64
	AccountID         int64
	Revision          int64
	IdentityHash      string
	UpstreamAccountID string
}

type CarpoolResetEvent struct {
	ID           int64
	Status       string
	Reason       string
	ResetCount   int
	SkippedCount int
}

type CarpoolResetInvalidation struct {
	EventID        int64
	SubscriptionID int64
	UserID         int64
	GroupID        int64
}

type CarpoolResetReceiptRecorder interface {
	RecordResetReceipt(context.Context, int64, string, string, time.Time, int) error
}

type CarpoolResetMonitorRepository interface {
	CarpoolResetReceiptRecorder
	Source(context.Context) (*CarpoolResetSource, error)
	ClearBaseline(context.Context) error
	Observe(context.Context, CarpoolResetSource, CarpoolResetObservation) (*CarpoolResetEvent, error)
	PendingEvents(context.Context) ([]int64, error)
	ApplyEvent(context.Context, int64, time.Time, time.Time) (*CarpoolResetEvent, error)
	PendingInvalidations(context.Context, int) ([]CarpoolResetInvalidation, error)
	CompleteInvalidation(context.Context, int64, int64) error
}
