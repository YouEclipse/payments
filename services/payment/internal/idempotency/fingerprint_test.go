package idempotency

import (
	"errors"
	"testing"
)

func TestFingerprintBindsOperationAndPayload(t *testing.T) {
	base := Fingerprint("create_payment_intent", []byte(`{"amount_minor":100,"currency":"USD"}`))
	same := Fingerprint("create_payment_intent", []byte(`{"amount_minor":100,"currency":"USD"}`))
	differentPayload := Fingerprint("create_payment_intent", []byte(`{"amount_minor":101,"currency":"USD"}`))
	differentOperation := Fingerprint("confirm_payment_intent:pi_1", []byte(`{}`))

	if !Equal(base, same) {
		t.Fatal("identical semantic requests must have the same fingerprint")
	}
	if Equal(base, differentPayload) {
		t.Fatal("different payload reused the same fingerprint")
	}
	if Equal(base, differentOperation) {
		t.Fatal("different API operation reused the same fingerprint")
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		err  error
	}{
		{name: "valid", key: "idem_01JABC", err: nil},
		{name: "missing", key: "", err: ErrKeyRequired},
		{name: "spaces", key: "bad key", err: ErrKeyInvalid},
		{name: "leading whitespace", key: " bad", err: ErrKeyInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateKey(test.key)
			if !errors.Is(err, test.err) {
				t.Fatalf("ValidateKey(%q)=%v, want %v", test.key, err, test.err)
			}
		})
	}
}
