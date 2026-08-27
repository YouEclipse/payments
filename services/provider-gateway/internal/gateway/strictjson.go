package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// decodeProviderJSON admits one bounded, unambiguous object. Provider bytes
// can become funds evidence, so duplicate names, case aliases, unknown fields,
// trailing values, and truncated bodies must fail before typed normalization.
func decodeProviderJSON(reader io.Reader, maxBytes int64, target any) error {
	payload, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil || len(payload) == 0 || int64(len(payload)) > maxBytes {
		return errors.New("provider JSON is empty, unreadable, or exceeds its size limit")
	}
	return decodeProviderJSONBytes(payload, target)
}

func decodeProviderJSONBytes(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	nodes := 0
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 || nodes >= 50_000 {
			return errors.New("provider JSON exceeds its complexity limit")
		}
		token, err := decoder.Token()
		if err != nil || token == nil {
			return errors.New("provider JSON is malformed or contains null")
		}
		nodes++
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || !canonicalProviderJSONKey(key) {
					return errors.New("provider JSON contains a non-canonical object field")
				}
				if _, duplicate := seen[key]; duplicate {
					return errors.New("provider JSON contains a duplicate object field")
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("provider JSON has an invalid object terminator")
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("provider JSON has an invalid array terminator")
			}
		default:
			return errors.New("provider JSON contains an invalid delimiter")
		}
		return nil
	}
	root, err := decoder.Token()
	if err != nil || root != json.Delim('{') {
		return errors.New("provider JSON must be one object")
	}
	if err := walkProviderJSONObject(decoder, 0, &nodes, walk); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("provider JSON must contain exactly one object")
	}

	typed := json.NewDecoder(bytes.NewReader(payload))
	typed.DisallowUnknownFields()
	if err := typed.Decode(target); err != nil {
		return errors.New("provider JSON contains an unknown field or invalid typed value")
	}
	if err := typed.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("provider JSON must contain exactly one object")
	}
	return nil
}

// The root opening delimiter is consumed separately so the same recursive
// walker can handle nested values without accepting an array root.
func walkProviderJSONObject(decoder *json.Decoder, depth int, nodes *int, walk func(int) error) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || !canonicalProviderJSONKey(key) {
			return errors.New("provider JSON contains a non-canonical object field")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("provider JSON contains a duplicate object field")
		}
		seen[key] = struct{}{}
		if err := walk(depth + 1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("provider JSON has an invalid object terminator")
	}
	return nil
}

func canonicalProviderJSONKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}
