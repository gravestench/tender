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

// CurrencyExponent returns the ISO 4217 minor-unit exponent. This table uses
// every code with a numeric minor unit in SIX List One, published 2026-09-17.
// Source: https://www.six-group.com/dam/download/financial-information/data-center/iso-currrency/lists/list-one.xml
// XML SHA-256: 33139b438657d1cee116ba737807ea71d19d6de4b90f799a09c56f0cc6a1b0ff.
// Provider-specific wire scaling and eligibility belong to the adapter.
func CurrencyExponent(currency string) (int, error) {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF",
		"UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0, nil
	case "AED", "AFN", "ALL", "AMD", "AOA", "ARS", "AUD", "AWG", "AZN", "BAM",
		"BBD", "BDT", "BMD", "BND", "BOB", "BOV", "BRL", "BSD", "BTN", "BWP",
		"BYN", "BZD", "CAD", "CDF", "CHE", "CHF", "CHW", "CNY", "COP", "COU",
		"CRC", "CUP", "CVE", "CZK", "DKK", "DOP", "DZD", "EGP", "ERN", "ETB",
		"EUR", "FJD", "FKP", "GBP", "GEL", "GHS", "GIP", "GMD", "GTQ", "GYD",
		"HKD", "HNL", "HTG", "HUF", "IDR", "ILS", "INR", "IRR", "JMD", "KES",
		"KGS", "KHR", "KPW", "KYD", "KZT", "LAK", "LBP", "LKR", "LRD", "LSL",
		"MAD", "MDL", "MGA", "MKD", "MMK", "MNT", "MOP", "MRU", "MUR", "MVR",
		"MWK", "MXN", "MXV", "MYR", "MZN", "NAD", "NGN", "NIO", "NOK", "NPR",
		"NZD", "PAB", "PEN", "PGK", "PHP", "PKR", "PLN", "QAR", "RON", "RSD",
		"RUB", "SAR", "SBD", "SCR", "SDG", "SEK", "SGD", "SHP", "SLE", "SOS",
		"SRD", "SSP", "STN", "SVC", "SYP", "SZL", "THB", "TJS", "TMT", "TOP",
		"TRY", "TTD", "TWD", "TZS", "UAH", "USD", "USN", "UYU", "UZS", "VED",
		"VES", "WST", "XAD", "XCD", "XCG", "YER", "ZAR", "ZMW", "ZWG":
		return 2, nil
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3, nil
	case "CLF", "UYW":
		return 4, nil
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
