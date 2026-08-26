package app

import (
	"context"
	"fmt"

	"github.com/YouEclipse/payments/services/payment/internal/clients"
	"github.com/YouEclipse/payments/services/payment/internal/domain"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

func (s *Service) buildRoutingRequest(ctx context.Context, intent domain.PaymentIntent, attemptID string) (clients.RoutingRequest, error) {
	legacy := clients.RoutingRequest{PaymentID: intent.ID, AttemptID: attemptID, MerchantID: intent.MerchantID,
		AmountMinor: intent.AmountMinor, Currency: intent.Currency, PaymentMethod: "card", Operation: "sale"}
	if s.contextBuilder == nil {
		return legacy, nil
	}
	snapshot, err := s.contextBuilder.Build(ctx, routingcontext.Input{PaymentID: intent.ID, AttemptID: attemptID,
		MerchantID: intent.MerchantID, AmountMinor: intent.AmountMinor, Currency: intent.Currency,
		CaptureMethod: string(intent.CaptureMethod), PaymentMethodToken: intent.PaymentMethodToken}, s.now().UTC())
	if err != nil {
		return clients.RoutingRequest{}, fmt.Errorf("%w: trusted routing context unavailable", clients.ErrUpstream)
	}
	return clients.RoutingRequest{Context: &snapshot}, nil
}
