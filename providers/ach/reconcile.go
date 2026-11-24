package ach

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/gravestench/tender"
)

// ProjectSettlement maps one host-authenticated Adyen ACH detail row into the
// generic ledger comparison format. The caller supplies the report's trusted
// observation time; this method does not authenticate or apply the report.
func ProjectSettlement(l SettlementLine, observedAt time.Time) (tender.SettlementLine, error) {
	if !l.valid() || observedAt.IsZero() {
		return tender.SettlementLine{}, tender.ErrValidation
	}
	identity := sha256.Sum256([]byte(l.Batch + "\x00" + l.PSP + "\x00" + l.Modification + "\x00" + l.Type))
	kind := tender.SettlementSettled
	if l.Type == "Chargeback" {
		kind = tender.SettlementReturned
	}
	return tender.SettlementLine{
		ID:                "ach." + hex.EncodeToString(identity[:]),
		ProviderReference: l.PSP,
		Kind:              kind,
		Currency:          "USD",
		GrossMinor:        l.Gross,
		FeeMinor:          l.Gross - l.Net,
		NetMinor:          l.Net,
		PayoutReference:   l.Batch,
		ObservedAt:        observedAt,
	}, nil
}
