// Package strictjson implements the bounded JSON admission rules shared by
// Context and Router-owned artifacts. It does not echo untrusted input.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const MaxBytes = 1 << 20

func Decode(payload []byte, target any) error {
	if len(payload) == 0 || len(payload) > MaxBytes {
		return errors.New("JSON body is empty or exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("JSON body must be one object")
	}
	budget := 20000
	if err := object(decoder, 0, &budget); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON body must contain exactly one object")
	}
	decoder = json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("JSON contains an unknown field or an invalid typed value")
	}
	return nil
}

func object(decoder *json.Decoder, depth int, budget *int) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !canonicalKey(key) {
			return errors.New("JSON object field names must use their canonical lowercase spelling")
		}
		if seen[key] {
			return errors.New("JSON contains a duplicate field")
		}
		seen[key] = true
		if err := value(decoder, depth+1, budget, key); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("invalid JSON object")
	}
	return nil
}

func value(decoder *json.Decoder, depth int, budget *int, key string) error {
	*budget--
	if depth > 24 || *budget < 0 {
		return errors.New("JSON exceeds its complexity limit")
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return errors.New("JSON null or malformed value is not allowed; omit optional fields")
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			return object(decoder, depth, budget)
		case '[':
			for decoder.More() {
				if err := value(decoder, depth+1, budget, ""); err != nil {
					return err
				}
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
			return nil
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if isTimeField(key) {
		text, ok := token.(string)
		if !ok || !strings.HasSuffix(text, "Z") {
			return errors.New("timestamps must be canonical UTC RFC3339Nano strings")
		}
		instant, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || instant.Format(time.RFC3339Nano) != text {
			return errors.New("timestamps must be canonical UTC RFC3339Nano strings")
		}
	}
	return nil
}

func canonicalKey(key string) bool {
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

func isTimeField(key string) bool {
	switch key {
	case "built_at", "observed_at", "expires_at", "published_at", "effective_from":
		return true
	default:
		return false
	}
}
