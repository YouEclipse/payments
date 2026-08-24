package simulator

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxRequestBody = 64 << 10

type Config struct {
	Environment   string
	ProviderID    string
	WebhookSecret string
	WebhookDelay  time.Duration
	TimeoutDelay  time.Duration
}

type Server struct {
	cfg        Config
	log        *slog.Logger
	httpClient *http.Client
	mu         sync.RWMutex
	operations map[string]operation
}

type SaleRequest struct {
	OperationID        string `json:"operation_id"`
	AmountMinor        int64  `json:"amount_minor"`
	Currency           string `json:"currency"`
	PaymentMethodToken string `json:"payment_method_token"`
	CaptureMethod      string `json:"capture_method"`
	Scenario           string `json:"scenario,omitempty"`
	WebhookURL         string `json:"webhook_url,omitempty"`
}

type SaleResponse struct {
	Provider          string `json:"provider"`
	OperationID       string `json:"operation_id"`
	ProviderReference string `json:"provider_reference,omitempty"`
	Status            string `json:"status"`
	FailureCode       string `json:"failure_code,omitempty"`
}

type operation struct {
	SaleResponse
	RequestHash string
	WebhookURL  string
	CreatedAt   time.Time
}

type webhookEvent struct {
	EventID           string    `json:"event_id"`
	Provider          string    `json:"provider"`
	OperationID       string    `json:"operation_id"`
	ProviderReference string    `json:"provider_reference,omitempty"`
	Status            string    `json:"status"`
	FailureCode       string    `json:"failure_code,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
}

func New(cfg Config, log *slog.Logger) (*Server, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Environment)) {
	case "local":
		cfg.Environment = "local"
	case "dev", "development":
		cfg.Environment = "development"
	case "test":
		cfg.Environment = "test"
	default:
		return nil, errors.New("provider simulator is restricted to local, development, or test environments")
	}
	if cfg.ProviderID == "" {
		return nil, errors.New("provider id is required")
	}
	if cfg.WebhookSecret == "" {
		return nil, errors.New("webhook secret is required")
	}
	if cfg.WebhookDelay <= 0 {
		cfg.WebhookDelay = 150 * time.Millisecond
	}
	if cfg.TimeoutDelay <= 0 {
		cfg.TimeoutDelay = 2 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:        cfg,
		log:        log,
		httpClient: &http.Client{Timeout: 2 * time.Second},
		operations: make(map[string]operation),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "# HELP provider_simulator_up Whether the provider simulator process is up.\n# TYPE provider_simulator_up gauge\nprovider_simulator_up 1\n")
	})
	mux.HandleFunc("POST /v1/sales", s.sale)
	mux.HandleFunc("GET /v1/sales/{idempotency_key}", s.query)
	return securityHeaders(mux)
}

func (s *Server) sale(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must be at most 200 characters")
		return
	}

	var req SaleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := validateSale(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	hash := requestHash(req)
	s.mu.Lock()
	existing, found := s.operations[key]
	if found {
		s.mu.Unlock()
		if existing.RequestHash != hash {
			writeError(w, http.StatusConflict, "idempotency_conflict", "the idempotency key was already used with a different request")
			return
		}
		writeJSON(w, http.StatusOK, existing.SaleResponse)
		return
	}

	result := operation{
		SaleResponse: SaleResponse{
			Provider:          s.cfg.ProviderID,
			OperationID:       req.OperationID,
			ProviderReference: "sim_" + randomHex(12),
		},
		RequestHash: hash,
		WebhookURL:  req.WebhookURL,
		CreatedAt:   time.Now().UTC(),
	}

	scenario := scenarioFor(req)
	switch scenario {
	case "approved", "timeout_then_succeeded":
		result.Status = "succeeded"
	case "declined", "timeout_then_declined":
		result.Status = "declined"
		result.FailureCode = "do_not_honor"
	default:
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "unsupported_scenario", "unsupported simulator scenario")
		return
	}
	s.operations[key] = result
	s.mu.Unlock()

	if result.WebhookURL != "" {
		go s.deliverWebhook(result)
	}
	if strings.HasPrefix(scenario, "timeout_then_") {
		time.Sleep(s.cfg.TimeoutDelay)
	}
	writeJSON(w, http.StatusOK, result.SaleResponse)
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("idempotency_key"))
	s.mu.RLock()
	result, ok := s.operations[key]
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "operation_not_found", "no operation exists for this idempotency key")
		return
	}
	writeJSON(w, http.StatusOK, result.SaleResponse)
}

func (s *Server) deliverWebhook(op operation) {
	time.Sleep(s.cfg.WebhookDelay)
	event := webhookEvent{
		EventID:           "evt_" + op.OperationID + "_" + op.Status,
		Provider:          s.cfg.ProviderID,
		OperationID:       op.OperationID,
		ProviderReference: op.ProviderReference,
		Status:            op.Status,
		FailureCode:       op.FailureCode,
		OccurredAt:        time.Now().UTC(),
	}
	body, err := json.Marshal(event)
	if err != nil {
		s.log.Error("marshal simulator webhook", "error", err)
		return
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.WebhookSecret))
	_, _ = mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))

	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, op.WebhookURL, bytes.NewReader(body))
		if err != nil {
			s.log.Error("build simulator webhook", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Provider-Signature", signature)
		resp, err := s.httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	s.log.Warn("simulator webhook delivery exhausted", "operation_id", op.OperationID, "provider", s.cfg.ProviderID)
}

func validateSale(req SaleRequest) error {
	if req.OperationID == "" {
		return errors.New("operation_id is required")
	}
	if req.AmountMinor <= 0 {
		return errors.New("amount_minor must be positive")
	}
	if len(req.Currency) != 3 || strings.ToUpper(req.Currency) != req.Currency {
		return errors.New("currency must be a three-letter uppercase ISO code")
	}
	if !strings.HasPrefix(req.PaymentMethodToken, "pm_sim_") {
		return errors.New("the simulator accepts only pm_sim_ tokens")
	}
	if req.CaptureMethod != "automatic" {
		return errors.New("phase one supports capture_method=automatic only")
	}
	return nil
}

func scenarioFor(req SaleRequest) string {
	if req.Scenario != "" {
		return req.Scenario
	}
	return strings.TrimPrefix(req.PaymentMethodToken, "pm_sim_")
}

func requestHash(req SaleRequest) string {
	material := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", req.OperationID, req.AmountMinor, req.Currency, req.PaymentMethodToken, req.CaptureMethod, scenarioFor(req))
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand: %w", err))
	}
	return hex.EncodeToString(b)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
