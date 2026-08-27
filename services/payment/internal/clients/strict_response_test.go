package clients

import (
	"strings"
	"testing"
)

func TestUniqueJSONRejectsAmbiguityAtEveryDepth(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid", `{"a":1,"nested":{"b":2},"items":[{"c":3}]}`, true},
		{"duplicate root", `{"a":1,"a":2}`, false},
		{"escaped duplicate", `{"a":1,"\u0061":2}`, false},
		{"duplicate nested", `{"nested":{"a":1,"a":2}}`, false},
		{"case alias root", `{"Status":"succeeded"}`, false},
		{"router nested case alias", `{"selected_candidate":{"Candidate_id":"conn_us"}}`, false},
		{"multiple values", `{} {}`, false},
		{"scalar root", `1`, true},
		{"malformed", `{"a":`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateUniqueJSON([]byte(test.payload))
			if (err == nil) != test.valid {
				t.Fatalf("error=%v valid=%v", err, test.valid)
			}
		})
	}
}

func TestVersionedDecisionEnvelopePreservesRequiredPresence(t *testing.T) {
	accepted := `{"decision_id":"rtd_1","selected_candidate":{},"candidate_order":[],"exclusions":[],"policy_version":"rules_v1","context_schema_version":"routing_context.v1","context_digest":"sha256:x","executable_until":"2026-08-26T01:00:01Z","rule_set_version":"rules_v1","rule_set_digest":"sha256:x","capability_catalog_version":"catalog_v1","capability_catalog_digest":"sha256:x","rule_hits":[],"eligibility":[],"reason_codes":[]}`
	rejectedV3 := `{"error":{},"decision_id":"rtd_1","candidate_order":[],"exclusions":[],"policy_version":"rules_v1","context_schema_version":"routing_context.v2","context_digest":"sha256:x","executable_until":"2026-08-26T01:00:01Z","rule_set_version":"rules_v1","rule_set_digest":"sha256:x","capability_catalog_version":"catalog_v1","capability_catalog_digest":"sha256:x","rule_hits":[],"eligibility":[],"risk":{},"reason_codes":[]}`
	for _, test := range []struct {
		name       string
		payload    string
		risk       bool
		accepted   bool
		wantAccept bool
	}{
		{"v2 success", accepted, false, true, true},
		{"v3 rejection", rejectedV3, true, false, true},
		{"missing array", strings.Replace(accepted, `"rule_hits":[],`, "", 1), false, true, false},
		{"null array", strings.Replace(accepted, `"rule_hits":[]`, `"rule_hits":null`, 1), false, true, false},
		{"success error", strings.Replace(accepted, `{"decision_id"`, `{"error":{},"decision_id"`, 1), false, true, false},
		{"rejection candidate", strings.Replace(rejectedV3, `"decision_id"`, `"selected_candidate":{},"decision_id"`, 1), true, false, false},
		{"v2 risk", strings.Replace(accepted, `"reason_codes"`, `"risk":{},"reason_codes"`, 1), false, true, false},
		{"v3 missing risk", strings.Replace(rejectedV3, `"risk":{},`, "", 1), true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateVersionedDecisionJSON([]byte(test.payload), test.risk, test.accepted)
			if (err == nil) != test.wantAccept {
				t.Fatalf("error=%v wantAccept=%v", err, test.wantAccept)
			}
		})
	}
}
