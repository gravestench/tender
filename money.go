package tender

import (
	"fmt"
	"math"
)

// Money is a signed number of minor units in a supported ISO 4217 currency.
// Its exported representation can be used directly by host persistence.
type Money struct {
	Minor    int64
	Currency string
}

// CurrencyExponent returns the number of decimal places in a minor unit.
// Tender's initial currency set is USD, JPY and KWD. Adapters may accept a
// narrower set and must explicitly map any provider-specific wire exponents.
func CurrencyExponent(currency string) (int, error) {
	switch currency {
	case "USD":
		return 2, nil
	case "JPY":
		return 0, nil
	case "KWD":
		return 3, nil
	default:
		return 0, fmt.Errorf("%w: unsupported currency", ErrValidation)
	}
}

func NewMoney(minor int64, currency string) (Money, error) {
	if _, err := CurrencyExponent(currency); err != nil {
		return Money{}, err
	}
	return Money{minor, currency}, nil
}

// ValidatePurchase requires a recognized currency and a strictly positive amount.
func (m Money) ValidatePurchase() error {
	if _, err := CurrencyExponent(m.Currency); err != nil {
		return err
	}
	if m.Minor <= 0 {
		return fmt.Errorf("%w: purchase amount must be positive", ErrValidation)
	}
	return nil
}

// Add returns the exact sum, rejecting mixed or invalid currencies and overflow.
func (m Money) Add(other Money) (Money, error) {
	if _, err := CurrencyExponent(m.Currency); err != nil {
		return Money{}, err
	}
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("%w: currency mismatch", ErrValidation)
	}
	if other.Minor > 0 && m.Minor > math.MaxInt64-other.Minor ||
		other.Minor < 0 && m.Minor < math.MinInt64-other.Minor {
		return Money{}, fmt.Errorf("%w: amount overflow", ErrValidation)
	}
	return Money{m.Minor + other.Minor, m.Currency}, nil
}
