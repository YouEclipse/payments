package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func TestPaymentBuildsContextFromOwnedIntentAndTrustedProfile(t *testing.T) {
	now := time.Now().UTC()
	builder, err := routingcontext.NewBuilder([]routingcontext.MerchantProfile{{MerchantID: "merchant_demo", EntityID: "merchant_demo_us", Country: "US", Version: "v1", ObservedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}}, routingcontext.SimulatorMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{contextBuilder: builder, now: func() time.Time { return now }}
	intent := domain.PaymentIntent{ID: "pi_demo", MerchantID: "merchant_demo", AmountMinor: 5200, Currency: "USD", CaptureMethod: domain.CaptureAutomatic, PaymentMethodToken: "pm_sim_approved"}
	req, err := service.buildRoutingRequest(t.Context(), intent, "att_demo")
	if err != nil {
		t.Fatal(err)
	}
	if req.Context == nil || req.Context.Base.PaymentID != intent.ID || req.Context.Base.AttemptID != "att_demo" || *req.Context.Merchant.Country.Value != "US" {
		t.Fatal("lost authoritative input")
	}
	payload, err := json.Marshal(req.Context)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), intent.PaymentMethodToken) {
		t.Fatal("credential leaked")
	}
	intent.MerchantID = "not_configured"
	if _, err := service.buildRoutingRequest(t.Context(), intent, "att_demo"); !errors.Is(err, clients.ErrUpstream) {
		t.Fatal("missing profile silently routed")
	}
}
