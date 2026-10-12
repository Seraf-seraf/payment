package invoice

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
	"time"
)

type SettlementResult struct {
	Record        Record
	Status        string
	ConfirmedPaid bool
	Changed       bool
	Evidence      *Evidence
	ReviewCode    string
}

// History contains original financial observations for this invoice/connection,
// including rejected evidence. Transport identifiers do not identify money.
func ApplySettlement(record Record, status string, history []Observation, observation Observation, now time.Time) (SettlementResult, error) {
	result, err := PrepareSettlement(record, status, observation, now)
	if err != nil {
		return result, err
	}
	inserted, conflict := result.Evidence != nil, false
	if result.Evidence != nil {
		for _, previous := range history {
			previous, key := canonicalSettlement(record, previous)
			if key != result.Evidence.FinancialKey {
				continue
			}
			hash, err := settlementHash(previous)
			if err != nil {
				return result, err
			}
			inserted, conflict = false, hash != result.Evidence.NormalizedHash
			break
		}
	}
	return CompleteSettlement(result, inserted, conflict, now)
}

// PrepareSettlement validates the fact without adding money before durable deduplication.
func PrepareSettlement(record Record, status string, observation Observation, now time.Time) (SettlementResult, error) {
	result := SettlementResult{Record: record, Status: status, ConfirmedPaid: record.CreditedMinor >= record.Request.AmountMinor && record.Request.AmountMinor > 0}
	result.Record.ReviewReasons = append([]string{}, record.ReviewReasons...)
	if record.Request.AmountMinor < 1 || record.Request.AmountMinor > MaxAmountMinor || record.CreditedMinor < 0 || record.CreditedMinor > MaxAmountMinor {
		return result, ErrInvalidRequest
	}
	if len(observation.References) > 1 {
		result.review("ambiguous_reference")
		return result, nil
	}
	eligible := true
	reject := func(reason string) { eligible = false; result.review(reason) }
	if observation.Kind == "correction" || observation.Kind == "financial_correction" {
		reject("financial_correction")
	}
	observation, key := canonicalSettlement(record, observation)
	if observation.Kind != "settlement" || !observation.FinalIncoming || observation.EvidenceMode != record.EvidenceMode || observation.AmountMinor < 1 || observation.AmountMinor > MaxAmountMinor || observation.RecipientAccount == "" || record.Connection.Beneficiary.Account == "" || observation.RecipientAccount != record.Connection.Beneficiary.Account {
		if result.ReviewCode == "" {
			reject("ambiguous_reference")
		}
	}
	if observation.Currency != record.Request.Currency {
		reject("currency_mismatch")
	}
	if observation.PayerINN != record.Request.Payer.INN {
		reject("payer_mismatch")
	}
	if key == "" {
		reject("ambiguous_reference")
	}
	if observation.Kind == "settlement" && observation.FinalIncoming && observation.EvidenceMode == record.EvidenceMode && observation.AmountMinor > 0 && observation.AmountMinor <= MaxAmountMinor && key != "" && len(observation.Currency) == 3 {
		hash, err := settlementHash(observation)
		if err != nil {
			return result, err
		}
		result.Evidence = &Evidence{PaymentID: record.PaymentID, ConnectionID: record.Connection.ID, FinancialKey: key, NormalizedHash: hash, EvidenceMode: observation.EvidenceMode, AmountMinor: observation.AmountMinor, Currency: observation.Currency, Eligible: eligible, RecordedAt: now.UTC(), ProviderOccurredAt: observation.ProviderOccurredAt}
	}
	return result, nil
}

// CompleteSettlement applies the persistence outcome; repeats never add coverage.
func CompleteSettlement(result SettlementResult, inserted, conflict bool, now time.Time) (SettlementResult, error) {
	now = now.UTC()
	if conflict {
		result.review("operation_changed")
	} else if inserted && result.Evidence != nil {
		result.Changed = true
		if result.Evidence.Eligible {
			if result.Record.CreditedMinor > MaxAmountMinor-result.Evidence.AmountMinor {
				return result, ErrAmountOverflow
			}
			result.Record.CreditedMinor += result.Evidence.AmountMinor
		}
	}
	if result.Changed {
		switch {
		case result.Record.CreditedMinor == 0:
			result.Record.FundingState = "none"
		case result.Record.CreditedMinor < result.Record.Request.AmountMinor:
			result.Record.FundingState = "partial"
		case result.Record.CreditedMinor == result.Record.Request.AmountMinor:
			result.Record.FundingState = "paid"
		default:
			result.Record.FundingState = "overpaid"
			result.requireReview("overpaid")
		}
		result.ConfirmedPaid = result.Record.CreditedMinor >= result.Record.Request.AmountMinor
		if result.ConfirmedPaid {
			if !now.Before(result.Record.Request.AcceptUntil) || result.Status == "canceled" || result.Status == "failed" {
				result.requireReview("acceptance_deadline_elapsed")
			}
			result.Status = "succeeded"
			if result.Record.ConfirmationRecordedAt == nil {
				result.Record.ConfirmationRecordedAt = &now
				if result.Evidence != nil {
					result.Record.PaymentConfirmedAt = result.Evidence.ProviderOccurredAt
				}
			}
		}
		result.finish(now)
	}
	return result, nil
}

func canonicalSettlement(record Record, o Observation) (Observation, string) {
	if o.ProviderOccurredAt != nil {
		occurred := o.ProviderOccurredAt.UTC()
		o.ProviderOccurredAt = &occurred
	}
	switch record.EvidenceMode {
	case "invoice_confirmation":
		if o.ExternalInvoiceID != "" && o.ExternalInvoiceID == record.ExternalInvoiceID {
			o.FinancialKey = "invoice:" + o.ExternalInvoiceID + ":final_settlement"
			return o, o.FinancialKey
		}
	case "bank_credits":
		if o.CanonicalFinancialKey && o.FinancialKey != "" {
			return o, o.FinancialKey
		}
	}
	return o, ""
}

func settlementHash(o Observation) ([32]byte, error) {
	data, err := json.Marshal(struct {
		Kind               string
		FinancialKey       string
		EvidenceMode       string
		AmountMinor        int64
		Currency           string
		RecipientAccount   string
		PayerINN           string
		FinalIncoming      bool
		ProviderOccurredAt *time.Time
	}{o.Kind, o.FinancialKey, o.EvidenceMode, o.AmountMinor, o.Currency, o.RecipientAccount, o.PayerINN, o.FinalIncoming, o.ProviderOccurredAt})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (r *SettlementResult) review(reason string) {
	r.ReviewCode = reason
	r.requireReview(reason)
}

func (r *SettlementResult) requireReview(reason string) {
	for _, existing := range r.Record.ReviewReasons {
		if existing == reason {
			return
		}
	}
	r.Record.ReviewReasons = append(r.Record.ReviewReasons, reason)
	sort.Strings(r.Record.ReviewReasons)
	r.Changed = true
}
func (r *SettlementResult) finish(now time.Time) {
	if r.Changed {
		r.Record.Revision++
		r.Record.UpdatedAt = now
	}
}
