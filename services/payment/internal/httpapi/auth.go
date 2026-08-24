package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var ErrUnauthorized = errors.New("unauthorized")

type Authenticator struct {
	merchantByDigest map[[sha256.Size]byte]string
	internalToken    []byte
}

func NewAuthenticator(merchantKeys map[string]string, internalToken string) *Authenticator {
	merchantByDigest := make(map[[sha256.Size]byte]string, len(merchantKeys))
	for merchantID, key := range merchantKeys {
		merchantByDigest[sha256.Sum256([]byte(key))] = merchantID
	}
	return &Authenticator{merchantByDigest: merchantByDigest, internalToken: []byte(internalToken)}
}

func (a *Authenticator) Merchant(request *http.Request) (string, error) {
	scheme, credential, ok := strings.Cut(request.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" {
		return "", ErrUnauthorized
	}
	merchantID, ok := a.merchantByDigest[sha256.Sum256([]byte(credential))]
	if !ok {
		return "", ErrUnauthorized
	}
	return merchantID, nil
}

func (a *Authenticator) Internal(request *http.Request) error {
	provided := []byte(request.Header.Get("X-Internal-Token"))
	if len(provided) != len(a.internalToken) || subtle.ConstantTimeCompare(provided, a.internalToken) != 1 {
		return ErrUnauthorized
	}
	return nil
}
