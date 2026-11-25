//go:build sandbox

package adyen

import (
	"strings"

	"github.com/gravestench/tender"
)

// NewACHSandboxQualification is available only in a sandbox-tagged binary.
// The ordinary NewACH constructor stays disabled for SANDBOX; this constructor
// cannot use an injected transport, clock, mock endpoint, or LIVE credentials.
// A successful adapter call is only a probe: signed provider events, an
// approved mandate, a complete report, and browser evidence are still needed.
func NewACHSandboxQualification(c Config) (*ACH, error) {
	if c.Environment != tender.EnvironmentSandbox || c.BaseURL != "https://checkout-test.adyen.com/v72" || c.Transport != nil || c.Now != nil || c.LiveActivated || c.MultiplePartialCaptures || strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.MerchantAccount) == "" {
		return nil, tender.ErrValidation
	}
	a, err := NewACH(c)
	if err != nil {
		return nil, err
	}
	a.sandboxQualification = true
	return a, nil
}
