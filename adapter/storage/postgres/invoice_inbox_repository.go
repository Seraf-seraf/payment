package postgres

import (
	"context"
	"encoding/json"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"time"
)

type InvoiceInboxRepository struct{ db queryer }

func newInvoiceInboxRepository(db queryer) *InvoiceInboxRepository {
	return &InvoiceInboxRepository{db: db}
}
func (r *InvoiceInboxRepository) Create(ctx context.Context, v d.Inbox) (bool, error) {
	payload, err := json.Marshal(v.Observation)
	if err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx, `INSERT INTO invoice_inbox(id,connection_id,event_key,raw_body_hash,normalized_payload,verification_kind,external_invoice_id,reference,state,received_at)
 VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),'received',$9) ON CONFLICT(connection_id,event_key,raw_body_hash) DO NOTHING`, v.ID, v.ConnectionID, v.EventKey, v.RawBodyHash[:], payload, v.VerificationKind, v.Observation.ExternalInvoiceID, v.Observation.Reference, v.ReceivedAt)
	return tag.RowsAffected() == 1, err
}

const inboxProjection = `id,connection_id,event_key,raw_body_hash,normalized_payload,verification_kind,state,received_at,processed_at,COALESCE(error_code,''),next_match_at`

func scanInbox(row pgx.Row) (d.Inbox, error) {
	var v d.Inbox
	var hash, payload []byte
	err := row.Scan(&v.ID, &v.ConnectionID, &v.EventKey, &hash, &payload, &v.VerificationKind, &v.State, &v.ReceivedAt, &v.ProcessedAt, &v.ErrorCode, &v.NextMatchAt)
	if err != nil {
		return v, err
	}
	copy(v.RawBodyHash[:], hash)
	err = json.Unmarshal(payload, &v.Observation)
	return v, err
}
func (r *InvoiceInboxRepository) Get(ctx context.Context, id uuid.UUID) (d.Inbox, error) {
	return scanInbox(r.db.QueryRow(ctx, `SELECT `+inboxProjection+` FROM invoice_inbox WHERE id=$1`, id))
}
func (r *InvoiceInboxRepository) SetState(ctx context.Context, id uuid.UUID, state, errorCode string, next *time.Time, now time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE invoice_inbox SET state=$2,error_code=NULLIF($3,''),next_match_at=$4,processed_at=CASE WHEN $2 IN ('processed','review') THEN $5::timestamptz ELSE NULL END WHERE id=$1`, id, state, errorCode, next, now)
	return err
}
func (r *InvoiceInboxRepository) Unmatched(ctx context.Context, connectionID, externalID, reference string, limit int) ([]d.Inbox, error) {
	return r.list(ctx, `SELECT `+inboxProjection+` FROM invoice_inbox WHERE state='unmatched' AND connection_id=$1 AND (($2<>'' AND external_invoice_id=$2) OR ($3<>'' AND reference=$3)) ORDER BY received_at,id LIMIT $4`, connectionID, externalID, reference, limit)
}
func (r *InvoiceInboxRepository) DueUnmatched(ctx context.Context, now time.Time, limit int) ([]d.Inbox, error) {
	return r.list(ctx, `SELECT `+inboxProjection+` FROM invoice_inbox WHERE state='unmatched' AND next_match_at<=$1 ORDER BY next_match_at,id LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
}
func (r *InvoiceInboxRepository) list(ctx context.Context, query string, args ...any) ([]d.Inbox, error) {
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []d.Inbox
	for rows.Next() {
		v, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
