package invoice

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxLines                    = 100
	MaxQuantity           int64 = 1_000_000
	MaxVATRateBasisPoints int32 = 10_000
)

var (
	ErrInvalidRequest = errors.New("invalid invoice request")
	ErrAmountOverflow = errors.New("invoice credited amount overflow")
	innPattern        = regexp.MustCompile(`^[0-9]{10}$`)
	kppPattern        = regexp.MustCompile(`^[0-9]{9}$`)
)

func NormalizeRequest(req Request) Request {

	req.OrderID = strings.TrimSpace(req.OrderID)
	req.Currency = strings.TrimSpace(req.Currency)
	req.Description = strings.TrimSpace(req.Description)
	req.Payer.Kind = strings.TrimSpace(req.Payer.Kind)
	req.Payer.Name = strings.TrimSpace(req.Payer.Name)
	req.Payer.INN = strings.TrimSpace(req.Payer.INN)
	req.Payer.KPP = strings.TrimSpace(req.Payer.KPP)
	req.Payer.LegalAddress = strings.TrimSpace(req.Payer.LegalAddress)
	req.Payer.Email = strings.TrimSpace(req.Payer.Email)
	req.AcceptUntil = req.AcceptUntil.UTC()
	req.Lines = append([]Line(nil), req.Lines...)
	for i := range req.Lines {
		line := &req.Lines[i]
		line.ID = strings.TrimSpace(line.ID)
		line.Name = strings.TrimSpace(line.Name)
		line.Unit = strings.TrimSpace(line.Unit)
		line.VAT.Kind = strings.TrimSpace(line.VAT.Kind)
		if line.VAT.RateBasisPoints != nil {
			rate := *line.VAT.RateBasisPoints
			line.VAT.RateBasisPoints = &rate
		}
	}
	return req
}

func ValidateRequest(req Request) error {
	return validateNormalizedRequest(NormalizeRequest(req))
}

// Deadline validation is separate so an existing intention can replay after expiry.
func ValidateNewDeadline(acceptUntil, now time.Time) error {
	if !acceptUntil.After(now) {
		return ErrInvalidRequest
	}
	return nil
}

func RequestHash(req Request) ([32]byte, error) {
	req = NormalizeRequest(req)
	if err := validateNormalizedRequest(req); err != nil {
		return [32]byte{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func textLength(value string) int { return utf8.RuneCountInString(value) }

func validateNormalizedRequest(req Request) error {
	if req.RequestID == uuid.Nil || textLength(req.OrderID) < 1 || textLength(req.OrderID) > 100 ||
		req.AmountMinor < 1 || req.AmountMinor > MaxAmountMinor || req.Currency != "RUB" ||
		textLength(req.Description) < 1 || textLength(req.Description) > 512 || req.Payer.Kind != "company" || textLength(req.Payer.Name) < 1 || textLength(req.Payer.Name) > 512 ||
		!innPattern.MatchString(req.Payer.INN) || !kppPattern.MatchString(req.Payer.KPP) ||
		textLength(req.Payer.LegalAddress) < 1 || textLength(req.Payer.LegalAddress) > 1000 || textLength(req.Payer.Email) > 254 ||
		len(req.Lines) < 1 || len(req.Lines) > MaxLines || req.AcceptUntil.IsZero() {
		return ErrInvalidRequest
	}
	if req.Payer.Email != "" {
		address, err := mail.ParseAddress(req.Payer.Email)
		if err != nil || address.Address != req.Payer.Email {
			return ErrInvalidRequest
		}
	}

	lineIDs := make(map[string]struct{}, len(req.Lines))
	var total int64
	for i := range req.Lines {
		line := req.Lines[i]
		line.ID = strings.TrimSpace(line.ID)
		line.Name = strings.TrimSpace(line.Name)
		line.Unit = strings.TrimSpace(line.Unit)
		if textLength(line.ID) < 1 || textLength(line.ID) > 100 || textLength(line.Name) < 1 || textLength(line.Name) > 512 ||
			line.Quantity < 1 || line.Quantity > MaxQuantity || line.UnitPriceMinor < 1 ||
			line.UnitPriceMinor > MaxAmountMinor || line.Quantity > math.MaxInt64/line.UnitPriceMinor ||
			line.AmountMinor != line.UnitPriceMinor*line.Quantity || line.AmountMinor > MaxAmountMinor {
			return ErrInvalidRequest
		}
		switch line.Unit {
		case "piece", "set", "service":
		default:
			return ErrInvalidRequest
		}
		if _, exists := lineIDs[line.ID]; exists {
			return ErrInvalidRequest
		}
		lineIDs[line.ID] = struct{}{}
		line.VAT.Kind = strings.TrimSpace(line.VAT.Kind)
		switch line.VAT.Kind {
		case "none":
			if line.VAT.RateBasisPoints != nil || line.VAT.AmountMinor != 0 {
				return ErrInvalidRequest
			}
		case "included":
			if line.VAT.RateBasisPoints == nil || *line.VAT.RateBasisPoints < 0 || *line.VAT.RateBasisPoints > MaxVATRateBasisPoints ||
				line.VAT.AmountMinor < 0 || line.VAT.AmountMinor > line.AmountMinor {
				return ErrInvalidRequest
			}
		default:
			return ErrInvalidRequest
		}
		if total > MaxAmountMinor-line.AmountMinor {
			return ErrInvalidRequest
		}
		total += line.AmountMinor
	}
	if total != req.AmountMinor {
		return ErrInvalidRequest
	}
	return nil
}
