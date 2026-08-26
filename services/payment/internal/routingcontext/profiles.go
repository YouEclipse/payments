package routingcontext

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// DecodeProfiles accepts one bounded, strict configuration artifact. In
// particular it does not allow duplicate object keys to replace an earlier
// country, identity or source version silently.
func DecodeProfiles(raw string) ([]MerchantProfile, error) {
	bad := errors.New("invalid merchant profile configuration")
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, bad
	}
	check := json.NewDecoder(bytes.NewBufferString(raw))
	if err := walkJSON(check, 0); err != nil {
		return nil, bad
	}
	if _, err := check.Token(); err != io.EOF {
		return nil, bad
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var profiles []MerchantProfile
	if err := decoder.Decode(&profiles); err != nil || len(profiles) == 0 || len(profiles) > 10000 {
		return nil, bad
	}
	if _, err := NewBuilder(profiles, UnavailableMetadata{}); err != nil {
		return nil, bad
	}
	return profiles, nil
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrInvalidContext
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalidContext
			}
			seen[name] = true
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidContext
		}
	case '[':
		for decoder.More() {
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidContext
		}
	default:
		return ErrInvalidContext
	}
	return nil
}
