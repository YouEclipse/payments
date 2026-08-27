package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// validateUniqueJSON rejects duplicate and non-canonical names before typed
// decoding. Go's default decoder otherwise keeps the last duplicate and
// matches struct fields case-insensitively, which would make an ambiguous wire
// representation look valid after projection.
func validateUniqueJSON(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	nodes := 0
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 || nodes > 100_000 {
			return errors.New("JSON structure exceeds limits")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
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
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if !canonicalResponseJSONKey(key) {
					return errors.New("non-canonical object member")
				}
				if _, duplicate := seen[key]; duplicate {
					return errors.New("duplicate object member")
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("invalid object terminator")
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("invalid array terminator")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func canonicalResponseJSONKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if !((character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

// validateVersionedDecisionJSON preserves presence semantics that a Go struct
// alone cannot: Router's canonical arrays are required even when empty, null
// is never a substitute, and success/rejection have disjoint envelopes.
func validateVersionedDecisionJSON(payload []byte, riskSchema, accepted bool) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return errors.New("versioned routing decision is not an object")
	}
	requireKind := func(name string, prefix byte) bool {
		raw, present := object[name]
		trimmed := bytes.TrimSpace(raw)
		return present && len(trimmed) > 0 && trimmed[0] == prefix
	}
	for _, name := range strings.Fields("decision_id policy_version context_schema_version context_digest executable_until rule_set_version rule_set_digest capability_catalog_version capability_catalog_digest") {
		if !requireKind(name, '"') {
			return errors.New("versioned routing decision omitted a string field")
		}
	}
	for _, name := range strings.Fields("candidate_order exclusions rule_hits eligibility reason_codes") {
		if !requireKind(name, '[') {
			return errors.New("versioned routing decision omitted a canonical array")
		}
	}
	if accepted {
		if !requireKind("selected_candidate", '{') {
			return errors.New("successful routing decision omitted its candidate")
		}
		if _, present := object["error"]; present {
			return errors.New("successful routing decision carried an error")
		}
	} else {
		if !requireKind("error", '{') {
			return errors.New("rejected routing decision omitted its error")
		}
		if _, present := object["selected_candidate"]; present {
			return errors.New("rejected routing decision carried a candidate")
		}
	}
	if riskSchema {
		if !requireKind("risk", '{') {
			return errors.New("v3 routing decision omitted risk evidence")
		}
		if _, present := object["optimization"]; present && !requireKind("optimization", '{') {
			return errors.New("v3 optimization evidence is not an object")
		}
	} else if _, risk := object["risk"]; risk {
		return errors.New("v2 routing decision carried v3 risk evidence")
	} else if _, optimization := object["optimization"]; optimization {
		return errors.New("v2 routing decision carried optimization evidence")
	}
	return nil
}
