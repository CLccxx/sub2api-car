package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type carpoolResetVoteRepository struct{ db *sql.DB }

func NewCarpoolResetVoteRepository(db *sql.DB) service.CarpoolResetVoteRepository {
	return &carpoolResetVoteRepository{db: db}
}

func (r *carpoolResetVoteRepository) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := (&carpoolResetMonitorRepository{db: r.db}).beginSourceGuardedTx(ctx)
	if err != nil {
		return nil, err
	}
	// Short predicate locks protect inserts, revocations and changed-back membership.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE users,user_subscriptions IN SHARE MODE NOWAIT`); err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_monitor(id) VALUES(1) ON CONFLICT DO NOTHING`)
	}
	if err == nil {
		var id int
		err = tx.QueryRowContext(ctx, `SELECT id FROM carpool_reset_monitor WHERE id=1 FOR UPDATE`).Scan(&id)
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func voteOwnerGroup(ctx context.Context, q carpoolResetQuerier, user, subscription int64) (int64, error) {
	var group int64
	err := q.QueryRowContext(ctx, `SELECT s.group_id FROM user_subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.user_id=$2 AND s.deleted_at IS NULL AND s.status='active' AND s.expires_at>clock_timestamp()
		AND s.starts_at<=clock_timestamp() AND u.deleted_at IS NULL AND u.status='active'`, subscription, user).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, service.ErrSubscriptionNotFound
	}
	return group, err
}

func voteEligibility(ctx context.Context, q carpoolResetQuerier, group int64) (*service.CarpoolVoteEligibility, error) {
	e := &service.CarpoolVoteEligibility{GroupID: group}
	err := q.QueryRowContext(ctx, `SELECT COALESCE(carpool_members,0) FROM groups WHERE id=$1 AND deleted_at IS NULL AND status='active'`, group).Scan(&e.RequiredCount)
	if errors.Is(err, sql.ErrNoRows) {
		e.BlockedReason = "not_configured"
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	e.Source, err = carpoolResetSource(ctx, q)
	if err != nil {
		return nil, err
	}
	if e.Source == nil || e.Source.GroupID != group {
		e.Source = nil
		e.BlockedReason = "account_unavailable"
	}
	if e.RequiredCount < 2 || e.RequiredCount > 5 {
		e.BlockedReason = "not_configured"
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT ON(s.user_id) s.user_id,s.id,s.starts_at FROM user_subscriptions s
		JOIN users u ON u.id=s.user_id WHERE s.group_id=$1 AND s.deleted_at IS NULL AND s.status='active'
		AND s.expires_at>clock_timestamp() AND s.starts_at<=clock_timestamp() AND u.deleted_at IS NULL AND u.status='active'
		ORDER BY s.user_id,s.id`, group)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m service.CarpoolVoteMember
		if err := rows.Scan(&m.UserID, &m.SubscriptionID, &m.StartsAt); err != nil {
			rows.Close()
			return nil, err
		}
		e.Members = append(e.Members, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(e.Members) != e.RequiredCount && e.BlockedReason == "" {
		e.BlockedReason = "member_count_mismatch"
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM carpool_vote_membership_changes WHERE group_id=$1`, group).Scan(&e.MembershipRevision); err != nil {
		return nil, err
	}
	if e.Source != nil {
		err = q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM carpool_account_reset_receipts WHERE account_id=$1),
			(SELECT count(*) FROM quota_reset_operations WHERE account_id=$1 AND status='succeeded')`, e.Source.AccountID).Scan(&e.ReceiptCount, &e.OperationCount)
	}
	return e, err
}

func voteEligibilityReason(old, current service.CarpoolVoteEligibility) string {
	if !sameCarpoolResetSource(old.Source, current.Source) {
		return "source_changed"
	}
	if old.ReceiptCount != current.ReceiptCount || old.OperationCount != current.OperationCount {
		return "admin_reset"
	}
	if current.BlockedReason != "" || old.RequiredCount != current.RequiredCount || old.MembershipRevision != current.MembershipRevision || len(old.Members) != len(current.Members) {
		return "membership_changed"
	}
	for i, m := range old.Members {
		n := current.Members[i]
		if m.UserID != n.UserID || m.SubscriptionID != n.SubscriptionID || !m.StartsAt.Equal(n.StartsAt) {
			return "membership_changed"
		}
	}
	return ""
}

func (r *carpoolResetVoteRepository) Eligibility(ctx context.Context, user, subscription int64) (*service.CarpoolVoteEligibility, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	group, err := voteOwnerGroup(ctx, tx, user, subscription)
	if err != nil {
		return nil, err
	}
	e, err := voteEligibility(ctx, tx, group)
	if err != nil {
		return nil, err
	}
	return e, tx.Commit()
}

const voteColumns = `id,eligibility,observation,status,expires_at,credit_id,request_id,dispatched_at IS NOT NULL,COALESCE(event_id,0)`

func scanVote(row *sql.Row) (*service.CarpoolResetVote, error) {
	p := &service.CarpoolResetVote{}
	var e, o []byte
	err := row.Scan(&p.ID, &e, &o, &p.Status, &p.ExpiresAt, &p.CreditID, &p.RequestID, &p.Dispatched, &p.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(e, &p.Eligibility); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(o, &p.Observation); err != nil {
		return nil, err
	}
	return p, nil
}

func validateVote(ctx context.Context, tx *sql.Tx, p *service.CarpoolResetVote, current *service.CarpoolVoteEligibility) error {
	if p == nil || p.Dispatched {
		return nil
	}
	if p.Status != "voting" && p.Status != "executing" {
		return closeUndispatchedVoteIntent(ctx, tx, p.ID)
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	reason := voteEligibilityReason(p.Eligibility, *current)
	status := "invalidated"
	if !p.ExpiresAt.After(now) {
		status = "expired"
		reason = "proposal_expired"
	} else if !p.Observation.ResetAt.After(now) {
		reason = "window_reset"
	}
	if reason == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET status=$2,reason=$3,updated_at=clock_timestamp() WHERE id=$1`, p.ID, status, reason)
	if err == nil {
		p.Status = status
		err = closeUndispatchedVoteIntent(ctx, tx, p.ID)
	}
	return err
}

func voteView(ctx context.Context, tx *sql.Tx, user int64, e *service.CarpoolVoteEligibility, p *service.CarpoolResetVote) (*service.ResetVoteView, error) {
	v := &service.ResetVoteView{RequiredCount: e.RequiredCount, MemberCount: len(e.Members), CanCreate: e.BlockedReason == ""}
	if e.BlockedReason != "" {
		v.BlockedReason = &e.BlockedReason
	}
	if p != nil {
		pv := &service.ResetVoteProposalView{ID: p.ID, Status: p.Status, RequiredCount: p.Eligibility.RequiredCount, ExpiresAt: p.ExpiresAt}
		err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(bool_or(user_id=$2),FALSE) FROM carpool_reset_vote_approvals WHERE proposal_id=$1`, p.ID, user).Scan(&pv.AgreedCount, &pv.MyVote)
		if err != nil {
			return nil, err
		}
		var reason sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT reason FROM carpool_reset_votes WHERE id=$1`, p.ID).Scan(&reason); err != nil {
			return nil, err
		}
		if reason.Valid {
			pv.Reason = &reason.String
		}
		pv.CanAgree = p.Status == "voting" && !pv.MyVote && e.BlockedReason == ""
		v.Proposal = pv
		if p.Status == "voting" || p.Status == "executing" || p.Status == "awaiting_confirmation" || p.Status == "applying" {
			v.CanCreate = false
		}
	}
	if e.Source != nil {
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quota_reset_operations WHERE account_id=$1 AND status='pending')
			OR EXISTS(SELECT 1 FROM carpool_reset_votes WHERE account_id=$1 AND status IN ('executing','awaiting_confirmation','applying'))`, e.Source.AccountID).Scan(&pending); err != nil {
			return nil, err
		}
		if pending {
			v.CanCreate = false
			reason := "reset_in_progress"
			v.BlockedReason = &reason
		}
		var voting bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM carpool_reset_votes WHERE account_id=$1 AND status='voting')`, e.Source.AccountID).Scan(&voting); err != nil {
			return nil, err
		}
		if voting {
			v.CanCreate = false
		}
	}
	return v, nil
}

func (r *carpoolResetVoteRepository) View(ctx context.Context, user, subscription int64) (*service.ResetVoteView, error) {
	return r.mutate(ctx, user, subscription, "", 0, nil, service.CarpoolResetObservation{})
}
func (r *carpoolResetVoteRepository) Creation(ctx context.Context, user, subscription int64, key string) (*service.ResetVoteView, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT proposal_id FROM carpool_reset_vote_requests WHERE created_by=$1 AND creation_key=$2::uuid`, user, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.mutate(ctx, user, subscription, key, 0, nil, service.CarpoolResetObservation{})
}
func (r *carpoolResetVoteRepository) Create(ctx context.Context, user, subscription int64, key string, expected service.CarpoolVoteEligibility, o service.CarpoolResetObservation) (*service.ResetVoteView, error) {
	return r.mutate(ctx, user, subscription, key, 0, &expected, o)
}
func (r *carpoolResetVoteRepository) Agree(ctx context.Context, user, subscription, proposal int64) (*service.ResetVoteView, error) {
	return r.mutate(ctx, user, subscription, "", proposal, nil, service.CarpoolResetObservation{})
}

func (r *carpoolResetVoteRepository) mutate(ctx context.Context, user, subscription int64, key string, agree int64, expected *service.CarpoolVoteEligibility, o service.CarpoolResetObservation) (*service.ResetVoteView, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	group, err := voteOwnerGroup(ctx, tx, user, subscription)
	if err != nil {
		return nil, err
	}
	e, err := voteEligibility(ctx, tx, group)
	if err != nil {
		return nil, err
	}
	p, err := scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE group_id=$1 ORDER BY id DESC LIMIT 1 FOR UPDATE`, group))
	if err != nil {
		return nil, err
	}
	if err = validateVote(ctx, tx, p, e); err != nil {
		return nil, err
	}
	if key != "" {
		replay, err := scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE id=(SELECT proposal_id FROM carpool_reset_vote_requests WHERE created_by=$1 AND creation_key=$2::uuid)`, user, key))
		if err != nil {
			return nil, err
		}
		if replay != nil {
			if replay.Eligibility.GroupID != group {
				return nil, service.ErrSubscriptionNotFound
			}
			if err = validateVote(ctx, tx, replay, e); err != nil {
				return nil, err
			}
			p = replay
		} else {
			view, err := voteView(ctx, tx, user, e, p)
			if err != nil {
				return nil, err
			}
			if view.CanCreate && expected != nil && voteEligibilityReason(*expected, *e) == "" {
				encoded, _ := json.Marshal(e)
				observed, _ := json.Marshal(o)
				p, err = scanVote(tx.QueryRowContext(ctx, `INSERT INTO carpool_reset_votes(group_id,account_id,created_by,creation_key,eligibility,observation,required_count)
					VALUES($1,$2,$3,$4::uuid,$5::jsonb,$6::jsonb,$7) RETURNING `+voteColumns, group, e.Source.AccountID, user, key, string(encoded), string(observed), e.RequiredCount))
				if err != nil {
					return nil, err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_vote_approvals(proposal_id,user_id) VALUES($1,$2)`, p.ID, user)
				if err != nil {
					return nil, err
				}
			} else if view.CanCreate {
				return nil, infraerrors.Conflict("RESET_VOTE_SOURCE_CHANGED", "Shared subscription changed; refresh and try again")
			}
			if p != nil && (p.Status == "voting" || p.Status == "executing" || p.Status == "awaiting_confirmation" || p.Status == "applying") {
				_, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_vote_requests(created_by,creation_key,proposal_id) VALUES($1,$2::uuid,$3) ON CONFLICT DO NOTHING`, user, key, p.ID)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if agree > 0 {
		if p == nil || p.ID != agree {
			// Terminal proposal retries are harmless, but never target another group's proposal.
			p, err = scanVote(tx.QueryRowContext(ctx, `SELECT `+voteColumns+` FROM carpool_reset_votes WHERE id=$1 AND group_id=$2 FOR UPDATE`, agree, group))
			if err != nil {
				return nil, err
			}
			if p == nil {
				return nil, service.ErrSubscriptionNotFound
			}
			if err = validateVote(ctx, tx, p, e); err != nil {
				return nil, err
			}
		}
		if p.Status == "voting" {
			_, err = tx.ExecContext(ctx, `INSERT INTO carpool_reset_vote_approvals(proposal_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.ID, user)
			if err != nil {
				return nil, err
			}
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM carpool_reset_vote_approvals WHERE proposal_id=$1`, p.ID).Scan(&count); err != nil {
				return nil, err
			}
			if count == p.Eligibility.RequiredCount {
				_, err = tx.ExecContext(ctx, `UPDATE carpool_reset_votes SET status='executing',updated_at=clock_timestamp() WHERE id=$1`, p.ID)
				if err != nil {
					return nil, err
				}
				p.Status = "executing"
			}
		}
	}
	v, err := voteView(ctx, tx, user, e, p)
	if err != nil {
		return nil, err
	}
	return v, tx.Commit()
}
