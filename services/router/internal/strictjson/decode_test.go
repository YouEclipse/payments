package strictjson_test

import (
	"strings"
	"testing"

	"github.com/YouEclipse/payments/services/router/internal/routingcontext"
	"github.com/YouEclipse/payments/services/router/internal/strictjson"
	"github.com/YouEclipse/payments/services/router/internal/testfixture"
)

func TestStrictJSONRejectsAmbiguousSecretAndUnboundedInput(t *testing.T) {
	valid := strings.TrimSpace(string(testfixture.ContextGolden))
	cases := map[string]string{
		"duplicate":             strings.Replace(valid, `"currency":"USD"`, `"currency":"EUR","currency":"USD"`, 1),
		"escaped duplicate":     strings.Replace(valid, `"currency":"USD"`, `"currency":"USD","cur\u0072ency":"EUR"`, 1),
		"case alias":            strings.Replace(valid, `"currency"`, `"Currency"`, 1),
		"null":                  strings.Replace(valid, `"category":"card"`, `"category":null`, 1),
		"null optional value":   strings.Replace(valid, `"state":"unknown"`, `"state":"unknown","value":null`, 1),
		"float amount":          strings.Replace(valid, `5200`, `5200.0`, 1),
		"overflow amount":       strings.Replace(valid, `5200`, `9223372036854775808`, 1),
		"unknown nested token":  strings.Replace(valid, `"category":"card"`, `"category":"card","payment_method_token":"private-token-do-not-log"`, 1),
		"unknown field name":    strings.Replace(valid, `"category":"card"`, `"category":"card","private-token-do-not-log":"x"`, 1),
		"policy injection":      strings.TrimSuffix(valid, "}") + `,"policy":{"version":"override"}}`,
		"time zone alias":       strings.Replace(valid, `2026-08-26T01:00:00Z`, `2026-08-26T01:00:00+00:00`, 1),
		"noncanonical fraction": strings.Replace(valid, `2026-08-26T01:00:00Z`, `2026-08-26T01:00:00.000Z`, 1),
		"second object":         valid + " {}",
		"array root":            "[" + valid + "]",
		"depth":                 `{"a":` + strings.Repeat(`{"a":`, 30) + `1` + strings.Repeat(`}`, 31),
		"size":                  strings.Repeat(" ", strictjson.MaxBytes+1) + valid,
		"budget":                `{"a":[` + strings.Repeat(`1,`, 20001) + `1]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var context routingcontext.Context
			err := strictjson.Decode([]byte(payload), &context)
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("wanted safe rejection, got %v", err)
			}
		})
	}
}

func TestStrictJSONAcceptsCanonicalNanoTimeAndTrailingWhitespace(t *testing.T) {
	payload := strings.Replace(string(testfixture.ContextGolden), `"built_at":"2026-08-26T01:00:00Z"`, `"built_at":"2026-08-26T01:00:00.123456789Z"`, 1) + "\n\t"
	var context routingcontext.Context
	if err := strictjson.Decode([]byte(payload), &context); err != nil {
		t.Fatal(err)
	}
	if err := context.Validate(); err != nil {
		t.Fatal(err)
	}
}
