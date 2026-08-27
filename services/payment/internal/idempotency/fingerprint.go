package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// FingerprintVersion labels the existing operation + NUL + typed JSON bytes.
// Changing it requires an explicit migration, not silently reusing old keys.
const FingerprintVersion = "command_sha256.v1"

var (
	ErrKeyRequired = errors.New("Idempotency-Key is required")
	ErrKeyTooLong  = errors.New("Idempotency-Key must not exceed 255 bytes")
	ErrKeyInvalid  = errors.New("Idempotency-Key contains unsupported characters")
	ErrKeyReused   = errors.New("Idempotency-Key was reused for a different request")
)

func ValidateKey(key string) error {
	if key == "" {
		return ErrKeyRequired
	}
	if len(key) > 255 {
		return ErrKeyTooLong
	}
	if strings.TrimSpace(key) != key {
		return ErrKeyInvalid
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return ErrKeyInvalid
		}
	}
	return nil
}

// Fingerprint binds a merchant-scoped idempotency key to one canonical API
// operation and payload. Reusing a key on another resource or with another
// amount therefore fails closed.
func Fingerprint(operation string, canonicalPayload []byte) []byte {
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write(canonicalPayload)
	return h.Sum(nil)
}

func Equal(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}

func Hex(value []byte) string {
	return hex.EncodeToString(value)
}
