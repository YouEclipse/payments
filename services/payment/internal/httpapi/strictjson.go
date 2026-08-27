package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// decodeJSONObject preserves the admitted bytes for evidence-bearing internal
// requests while rejecting every ambiguous representation before typed decode.
// Unknown values are never echoed in the returned error.
func decodeJSONObject(response http.ResponseWriter, request *http.Request, target any) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBody+1))
	if err != nil || len(payload) == 0 || len(payload) > maxRequestBody {
		return nil, errors.New("invalid JSON body")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	root, err := decoder.Token()
	if err != nil || root != json.Delim('{') {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	nodes := 0
	if err := walkJSONObject(decoder, 0, &nodes); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	typed := json.NewDecoder(bytes.NewReader(payload))
	typed.DisallowUnknownFields()
	if err := typed.Decode(target); err != nil {
		return nil, errors.New("JSON contains an unknown field or invalid typed value")
	}
	if err := typed.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	return payload, nil
}

func walkJSONObject(decoder *json.Decoder, depth int, nodes *int) error {
	if depth > 32 || *nodes >= 50_000 {
		return errors.New("JSON exceeds its complexity limit")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || !canonicalJSONKey(key) {
			return errors.New("JSON contains a non-canonical object field")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("JSON contains a duplicate object field")
		}
		seen[key] = struct{}{}
		if err := walkJSONValue(decoder, depth+1, nodes); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("invalid JSON object")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, depth int, nodes *int) error {
	if depth > 32 || *nodes >= 50_000 {
		return errors.New("JSON exceeds its complexity limit")
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return errors.New("JSON null or malformed values are not allowed")
	}
	*nodes++
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		return walkJSONObject(decoder, depth, nodes)
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1, nodes); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
		return nil
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func canonicalJSONKey(key string) bool {
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
