package routingcontext

const (
	KindString  = "string"
	KindInteger = "integer"
)

// Value is the bounded rule field registry's typed view. Unknown observations
// never expose an old Value as a known fact, including under negation.
type Value struct {
	Kind    string
	State   string
	String  string
	Integer int64
}

func FieldKind(field string) (string, bool) {
	switch field {
	case "base.amount_minor":
		return KindInteger, true
	case "base.payment_id", "base.attempt_id", "base.merchant_id", "base.currency", "base.payment_method", "base.operation", "base.capture_method", "merchant.entity_id", "merchant.country", "payment_method_context.category", "payment_method_context.network", "payment_method_context.issuer_country", "payment_method_context.credential_domain":
		return KindString, true
	default:
		return "", false
	}
}

// ValidFieldString validates rule literals against the same field vocabulary
// as Context admission. A misspelled currency/network/action must not silently
// compile into a predicate that can never match a valid request.
func ValidFieldString(field, value string) bool {
	kind, exists := FieldKind(field)
	if !exists || kind != KindString {
		return false
	}
	switch field {
	case "base.currency":
		return currency.MatchString(value)
	case "merchant.country", "payment_method_context.issuer_country":
		return country.MatchString(value)
	case "base.payment_method", "payment_method_context.category":
		return value == "card"
	case "base.operation":
		return oneOf(value, "sale", "authorize")
	case "base.capture_method":
		return oneOf(value, "automatic", "manual")
	case "payment_method_context.network":
		return oneOf(value, "visa", "mastercard", "amex", "discover", "jcb", "unionpay", "diners")
	default:
		return ValidIdentifier(value)
	}
}

func (c Context) Lookup(field string) Value {
	known := func(value string) Value { return Value{Kind: KindString, State: StateKnown, String: value} }
	observed := func(value Observed) Value {
		result := Value{Kind: KindString, State: StateUnknown}
		if value.State == StateKnown && value.Value != nil && value.ExpiresAt != nil && c.BuiltAt.Before(*value.ExpiresAt) {
			result.State, result.String = StateKnown, *value.Value
		}
		return result
	}
	switch field {
	case "base.payment_id":
		return known(c.Base.PaymentID)
	case "base.attempt_id":
		return known(c.Base.AttemptID)
	case "base.merchant_id":
		return known(c.Base.MerchantID)
	case "base.amount_minor":
		return Value{Kind: KindInteger, State: StateKnown, Integer: c.Base.AmountMinor}
	case "base.currency":
		return known(c.Base.Currency)
	case "base.payment_method":
		return known(c.Base.PaymentMethod)
	case "base.operation":
		return known(c.Base.Operation)
	case "base.capture_method":
		return known(c.Base.CaptureMethod)
	case "merchant.entity_id":
		return known(c.Merchant.EntityID)
	case "merchant.country":
		return observed(c.Merchant.Country)
	case "payment_method_context.category":
		return known(c.PaymentMethod.Category)
	case "payment_method_context.network":
		return observed(c.PaymentMethod.Network)
	case "payment_method_context.issuer_country":
		return observed(c.PaymentMethod.IssuerCountry)
	case "payment_method_context.credential_domain":
		return observed(c.PaymentMethod.CredentialDomain)
	default:
		return Value{State: StateUnknown}
	}
}
