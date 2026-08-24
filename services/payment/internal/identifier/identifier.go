package identifier

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func New(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return prefix + hex.EncodeToString(entropy[:]), nil
}
