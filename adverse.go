package tender

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AdverseKey identifies one authenticated provider delivery on one merchant
// route. It is also the durable review item's idempotency key.
type AdverseKey struct {
	Scope                     Scope
	Provider, MerchantAccount string
	ProviderEventID           string
}

// AdverseNotice is an auditable host review item. It does not change payment
// funds or reverse a previously issued fulfillment notice. The provider
// projector derives references from persisted verified evidence; the engine
// checks them against the bound payment and any named operation.
type AdverseNotice struct {
	Scope                     Scope
	Provider, MerchantAccount string
	PaymentID, OperationID    string
	ProviderEventID, Kind     string
	ProviderReference         string
	OriginalReference         string
	ModificationReference     string
	Amount                    Money
	ObservedAt                time.Time
	Decision                  *AdverseDecision `json:",omitempty"`
}

func (n AdverseNotice) Key() AdverseKey {
	return AdverseKey{Scope: n.Scope, Provider: n.Provider, MerchantAccount: n.MerchantAccount, ProviderEventID: n.ProviderEventID}
}

// AdverseDecision is an append-only review transition. Rationale is a short
// redacted code, not customer, card, bank or provider payload data.
type AdverseDecision struct {
	Disposition, ReviewerID, Rationale string
	ReviewedAt                         time.Time
}

// AdverseReviewTx is available on stores supporting durable adverse review.
// Enqueue requires an engine-issued claim for the active verified receipt
// callback in this transaction. The zero claim cannot authorize an insert.
type AdverseReviewTx interface {
	EnqueueAdverse(AdverseNotice, AdverseClaim) error
	ReviewAdverse(AdverseKey, AdverseDecision) error
}

// AdverseClaim is a sealed, transaction-scoped authorization for a notice.
// Only Tender's Engine can construct a valid claim after binding verified
// evidence to the payment and route. A host or adapter can pass its zero value
// but cannot create a valid claim or turn an arbitrary inbox row into proof.
type AdverseClaim struct {
	key    VerifiedEventKey
	notice AdverseNotice
	valid  bool
}

// ValidFor lets a Store check that the claim belongs to the current verified
// receipt callback and the exact normalized notice it is about to persist.
func (c AdverseClaim) ValidFor(key VerifiedEventKey, notice AdverseNotice) bool {
	return c.valid && c.key == key && c.notice == notice
}

// AdverseEvidenceReader exposes only notices whose verified inbox receipt is
// committed as processed. Reconciliation must not trust a report or a directly
// inserted notice alone as proof of a return or dispute.
type AdverseEvidenceReader interface {
	AdverseEvidence(Scope, string, string, string) ([]AdverseNotice, error)
}

func boundedOpaque(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '@' || r == '<' || r == '>' {
			return false
		}
	}
	return true
}

func (k AdverseKey) Validate() error {
	if err := k.Scope.Validate(); err != nil {
		return err
	}
	if !boundedOpaque(k.Scope.MerchantID, 200) || !boundedOpaque(k.Provider, 80) ||
		!boundedOpaque(k.MerchantAccount, 200) || !boundedOpaque(k.ProviderEventID, 200) {
		return ErrValidation
	}
	return nil
}

func (n AdverseNotice) Validate() error {
	if err := n.Key().Validate(); err != nil {
		return err
	}
	if !boundedOpaque(n.PaymentID, 200) || !boundedOpaque(n.Kind, 80) ||
		!boundedOpaque(n.OriginalReference, 200) || !boundedOpaque(n.ModificationReference, 200) ||
		(n.OperationID != "" && !boundedOpaque(n.OperationID, 200)) ||
		(n.ProviderReference != "" && !boundedOpaque(n.ProviderReference, 200)) ||
		n.ObservedAt.IsZero() || n.Decision != nil || n.Amount.ValidatePurchase() != nil {
		return ErrValidation
	}
	return nil
}

func (d AdverseDecision) Normalize() (AdverseDecision, error) {
	if !boundedOpaque(d.Disposition, 48) || !boundedOpaque(d.ReviewerID, 100) ||
		!boundedOpaque(d.Rationale, 128) || d.ReviewedAt.IsZero() {
		return AdverseDecision{}, ErrValidation
	}
	d.ReviewedAt = d.ReviewedAt.Round(0).UTC()
	return d, nil
}

// ProcessVerifiedAdverseEvent consumes an already authenticated receipt. The
// projector is trusted provider code: it must parse and corroborate its own
// wire format, including the modification reference. The core independently
// binds the normalized notice to this receipt, route and stored payment. A
// notice-only event cannot change payment funds or the fulfillment outbox.
func (e *Engine) ProcessVerifiedAdverseEvent(ctx context.Context, id string, project func(EventRecords, json.RawMessage) (AdverseNotice, error)) error {
	if !boundedOpaque(id, 200) || project == nil {
		return ErrValidation
	}
	key := AdverseKey{Scope: e.config.Scope, Provider: e.config.ProviderName, MerchantAccount: e.config.MerchantAccount, ProviderEventID: id}
	return e.config.Store.Transact(ctx, func(tx Tx) error {
		inbox, ok := tx.(VerifiedEventTx)
		if !ok {
			return ErrUnsupported
		}
		review, ok := tx.(AdverseReviewTx)
		if !ok {
			return ErrUnsupported
		}
		verifiedKey := VerifiedEventKey{Scope: key.Scope, Provider: key.Provider, MerchantAccount: key.MerchantAccount, ID: key.ProviderEventID}
		return inbox.ProcessVerifiedEvent(verifiedKey, e.config.Now().UTC(), func(evidence json.RawMessage) error {
			n, err := project(tx, evidence)
			if err != nil {
				return err
			}
			if err := n.Validate(); err != nil {
				return err
			}
			if n.Key() != key {
				return fmt.Errorf("%w: adverse receipt identity mismatch", ErrConflict)
			}
			p, err := tx.Payment(n.Scope, n.PaymentID)
			if err != nil {
				return err
			}
			if err := e.route(p); err != nil {
				return err
			}
			if p.ProviderReference == "" || n.OriginalReference != p.ProviderReference ||
				n.Amount.Currency != p.Amount.Currency || n.Amount.Minor > p.Amount.Minor {
				return fmt.Errorf("%w: adverse payment correlation mismatch", ErrConflict)
			}
			if n.OperationID != "" {
				op, err := tx.Operation(n.Scope, n.OperationID)
				if err != nil {
					return err
				}
				if op.PaymentID != p.ID || op.AttemptID != p.AttemptID ||
					op.ProviderReference == "" || op.ProviderReference != n.ModificationReference {
					return fmt.Errorf("%w: adverse operation correlation mismatch", ErrConflict)
				}
			}
			n.ObservedAt = n.ObservedAt.Round(0).UTC()
			return review.EnqueueAdverse(n, AdverseClaim{key: verifiedKey, notice: n, valid: true})
		})
	})
}
