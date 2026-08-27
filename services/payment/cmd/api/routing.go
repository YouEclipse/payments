package main

import (
	"errors"

	"github.com/YouEclipse/payments/services/payment/internal/config"
	"github.com/YouEclipse/payments/services/payment/internal/routingcontext"
)

// This is an explicit reader-first rollout choice, not a response-driven
// fallback. An unavailable risk source fails confirmation before an Attempt or
// Provider Operation exists. A real risk adapter is intentionally not claimed.
func routingContextBuilder(settings config.Config) (*routingcontext.Builder, error) {
	if settings.RouterAPIVersion == "v1" {
		return nil, nil
	}
	if settings.RouterAPIVersion != "v2" && settings.RouterAPIVersion != "v3" {
		return nil, errors.New("unsupported Router API version")
	}
	var metadata routingcontext.MetadataSource = routingcontext.UnavailableMetadata{}
	if routingcontext.IsLocalEnvironment(settings.Environment) {
		metadata = routingcontext.SimulatorMetadata{}
	}
	builder, err := routingcontext.NewBuilder(settings.MerchantProfiles, metadata)
	if err != nil || settings.RouterAPIVersion == "v2" {
		return builder, err
	}
	var source routingcontext.RiskSource
	switch settings.RiskMode {
	case "local_fixture":
		if !routingcontext.IsLocalEnvironment(settings.Environment) {
			return nil, errors.New("local risk fixture is forbidden outside local/test")
		}
		source = routingcontext.DevelopmentRisk{}
	case "unavailable":
		source = routingcontext.UnavailableRisk{}
	default:
		return nil, errors.New("unsupported risk source")
	}
	return builder.WithRisk(source)
}
