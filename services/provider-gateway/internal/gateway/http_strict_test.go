package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayCommandJSONHasOneCanonicalRepresentation(t *testing.T) {
	valid := `{
		"operation_id":"op_strict","attempt_id":"att_strict",
		"provider_connection_id":"provider_a","provider_connection_revision":"provider_a_v1",
		"provider_namespace_id":"provider_a","provider":"provider_a",
		"amount_minor":100,"currency":"USD","payment_method_token":"pm_test",
		"capture_method":"automatic","scenario":""
	}`
	tests := []struct {
		name string
		body string
	}{
		{name: "duplicate", body: strings.Replace(valid, `"operation_id":"op_strict"`, `"operation_id":"op_strict","operation_id":"op_other"`, 1)},
		{name: "case alias", body: strings.Replace(valid, `"operation_id"`, `"Operation_ID"`, 1)},
		{name: "unknown field", body: strings.Replace(valid, `"scenario":""`, `"scenario":"","extra":true`, 1)},
		{name: "trailing object", body: valid + `{}`},
		{name: "null", body: strings.Replace(valid, `"scenario":""`, `"scenario":null`, 1)},
		{name: "array root", body: `[` + valid + `]`},
		{name: "oversized", body: `{"operation_id":"` + strings.Repeat("x", (64<<10)+1) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/internal/v1/provider-operations", strings.NewReader(test.body))
			var input OperationRequest
			if err := decodeJSON(httptest.NewRecorder(), request, &input); err == nil {
				t.Fatalf("ambiguous Gateway command was accepted: %s", test.name)
			}
		})
	}

	request := httptest.NewRequest("POST", "/internal/v1/provider-operations", strings.NewReader(valid))
	var input OperationRequest
	if err := decodeJSON(httptest.NewRecorder(), request, &input); err != nil {
		t.Fatalf("canonical Gateway command rejected: %v", err)
	}
	if input.OperationID != "op_strict" || input.AmountMinor != 100 {
		t.Fatalf("canonical Gateway command decoded incorrectly: %+v", input)
	}
}
