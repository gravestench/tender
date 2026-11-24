package tender

import (
	"regexp"
	"time"
)

// ErrorKind is a stable category usable with errors.Is.
type ErrorKind string

const (
	ErrUnsupported    ErrorKind = "unsupported"
	ErrDeclined       ErrorKind = "declined"
	ErrValidation     ErrorKind = "validation"
	ErrAuthentication ErrorKind = "authentication"
	ErrRateLimit      ErrorKind = "rate_limit"
	ErrTransport      ErrorKind = "transport"
	// ErrNotDispatched means a prerequisite failed before the provider mutation could have an effect.
	ErrNotDispatched  ErrorKind = "not_dispatched"
	ErrUnknownOutcome ErrorKind = "unknown_outcome"
	ErrConflict       ErrorKind = "conflict"
)

func (k ErrorKind) Error() string { return string(k) }

// ProviderError carries a category and a provider's public status code. An
// adapter may supply RetryAfter when the provider returns a retry interval.
type ProviderError struct {
	Kind           ErrorKind
	ProviderStatus string
	RetryAfter     time.Duration
}

func NewProviderError(kind ErrorKind, status string) *ProviderError {
	return &ProviderError{Kind: kind, ProviderStatus: sanitizeStatus(status)}
}

func (e *ProviderError) Error() string { return e.Kind.Error() }
func (e *ProviderError) Unwrap() error { return e.Kind }

// Adapters must pass only documented public codes. Discard entire malformed
// codes to avoid turning free-form provider messages into stored fragments.
var statusCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

func sanitizeStatus(s string) string {
	if statusCode.MatchString(s) {
		return s
	}
	return ""
}
