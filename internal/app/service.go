package app

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spsp4755/ai-gateway/internal/config"
	"github.com/spsp4755/ai-gateway/internal/storage"
)

type Service struct {
	cfg    config.Config
	store  *storage.Store
	client *http.Client
	rng    *rand.Rand
	mu     sync.Mutex
}

type rankedBackend struct {
	backend storage.Backend
	score   int
}

type ProxyResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Streamed   bool
}

func NewService(cfg config.Config, store *storage.Store) *Service {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.UpstreamInsecureSkipVerify,
		},
	}
	return &Service{
		cfg:   cfg,
		store: store,
		client: &http.Client{
			Transport: transport,
		},
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *Service) StartHealthMonitor(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.HealthcheckInterval)
	defer ticker.Stop()

	_ = s.ProbeAllBackends(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ProbeAllBackends(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("health monitor: %v", err)
			}
		}
	}
}

func (s *Service) ProbeAllBackends(ctx context.Context) error {
	backends, err := s.store.ListBackends(ctx)
	if err != nil {
		return err
	}
	for _, backend := range backends {
		if err := s.ProbeBackend(ctx, backend.ID); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("probe backend %s: %v", backend.Name, err)
		}
	}
	return nil
}

func (s *Service) ProbeBackend(ctx context.Context, backendID int64) error {
	backend, err := s.store.GetBackendByID(ctx, backendID)
	if err != nil {
		return err
	}
	if backend == nil {
		return fmt.Errorf("backend %d not found", backendID)
	}
	healthStatus, lastError := s.checkBackend(ctx, *backend)
	return s.store.UpdateBackendHealth(ctx, backendID, healthStatus, lastError)
}

func (s *Service) SelectAccessibleModels(ctx context.Context, auth *storage.APIKeyAuth) ([]storage.LogicalModel, error) {
	models, err := s.store.ListLogicalModels(ctx)
	if err != nil {
		return nil, err
	}
	if auth == nil || len(auth.AllowedModels) == 0 {
		filtered := make([]storage.LogicalModel, 0, len(models))
		for _, model := range models {
			if model.Status == "active" {
				filtered = append(filtered, model)
			}
		}
		return filtered, nil
	}

	allowed := map[string]struct{}{}
	for _, name := range auth.AllowedModels {
		allowed[name] = struct{}{}
	}
	filtered := make([]storage.LogicalModel, 0, len(models))
	for _, model := range models {
		if model.Status != "active" {
			continue
		}
		if _, ok := allowed[model.Name]; ok {
			filtered = append(filtered, model)
		}
	}
	return filtered, nil
}

func (s *Service) ProxyChatCompletion(w http.ResponseWriter, r *http.Request, auth *storage.APIKeyAuth) error {
	start := time.Now()
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = "req-" + randomToken(24)
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Request body must be valid JSON")
		return nil
	}

	modelName, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(modelName) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Request body must include a non-empty model")
		return nil
	}
	modelName = strings.TrimSpace(modelName)

	if auth != nil && len(auth.AllowedModels) > 0 && !contains(auth.AllowedModels, modelName) {
		writeJSONError(w, http.StatusForbidden, "permission_denied", "API key cannot access the requested model")
		return nil
	}

	model, err := s.store.GetLogicalModelByName(r.Context(), modelName)
	if err != nil {
		return err
	}
	if model == nil || model.Status != "active" {
		writeJSONError(w, http.StatusNotFound, "not_found_error", "Requested model is not registered")
		return nil
	}

	streamRequested, _ := payload["stream"].(bool)
	backends, err := s.store.ListBackendsForModel(r.Context(), modelName)
	if err != nil {
		return err
	}
	attemptOrder := s.buildAttemptOrder(backends)
	if len(attemptOrder) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "No healthy backend is available for the requested model")
		_ = s.store.LogRequest(r.Context(), storage.RequestLog{
			RequestID:        requestID,
			APIKeyName:       apiKeyName(auth),
			LogicalModelName: modelName,
			StatusCode:       http.StatusServiceUnavailable,
			LatencyMs:        time.Since(start).Milliseconds(),
			Success:          false,
			ErrorMessage:     "no eligible backend",
		})
		return nil
	}

	var lastError string
	for _, backend := range attemptOrder {
		timeout := s.cfg.RequestTimeout
		if backend.TimeoutMs > 0 {
			timeout = time.Duration(backend.TimeoutMs) * time.Millisecond
		}
		attemptCtx, cancel := context.WithTimeout(r.Context(), timeout)
		payload["model"] = backend.UpstreamModelName
		proxyBytes, err := json.Marshal(payload)
		if err != nil {
			cancel()
			return err
		}

		if streamRequested {
			statusCode, attemptErr := s.streamBackend(w, attemptCtx, backend, proxyBytes, requestID)
			cancel()
			if attemptErr != nil {
				lastError = attemptErr.Error()
				if ctxErr := r.Context().Err(); ctxErr != nil {
					return ctxErr
				}
				continue
			}
			_ = s.store.LogRequest(r.Context(), storage.RequestLog{
				RequestID:        requestID,
				APIKeyName:       apiKeyName(auth),
				LogicalModelName: modelName,
				BackendName:      backend.Name,
				BackendURL:       backend.BaseURL,
				Streamed:         true,
				StatusCode:       statusCode,
				LatencyMs:        time.Since(start).Milliseconds(),
				Success:          statusCode < 500,
				ErrorMessage:     lastError,
			})
			return nil
		}

		result, usage, attemptErr := s.callBackend(attemptCtx, backend, proxyBytes, requestID)
		cancel()
		if attemptErr != nil {
			lastError = attemptErr.Error()
			if ctxErr := r.Context().Err(); ctxErr != nil {
				return ctxErr
			}
			continue
		}
		copyHeaders(w.Header(), result.Headers)
		w.Header().Set("X-Request-Id", requestID)
		w.WriteHeader(result.StatusCode)
		if _, err := w.Write(result.Body); err != nil {
			return err
		}
		_ = s.store.LogRequest(r.Context(), storage.RequestLog{
			RequestID:        requestID,
			APIKeyName:       apiKeyName(auth),
			LogicalModelName: modelName,
			BackendName:      backend.Name,
			BackendURL:       backend.BaseURL,
			Streamed:         false,
			StatusCode:       result.StatusCode,
			LatencyMs:        time.Since(start).Milliseconds(),
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			Success:          result.StatusCode < 500,
			ErrorMessage:     lastError,
		})
		return nil
	}

	writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "All configured backends failed for the requested model")
	_ = s.store.LogRequest(r.Context(), storage.RequestLog{
		RequestID:        requestID,
		APIKeyName:       apiKeyName(auth),
		LogicalModelName: modelName,
		StatusCode:       http.StatusServiceUnavailable,
		LatencyMs:        time.Since(start).Milliseconds(),
		Success:          false,
		ErrorMessage:     lastError,
	})
	return nil
}

type usageSummary struct {
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
}

func (s *Service) streamBackend(w http.ResponseWriter, ctx context.Context, backend storage.Backend, payload []byte, requestID string) (int, error) {
	upstreamURL := backend.BaseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Request-Id", requestID)
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("upstream %s returned %d: %s", backend.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	copyHeaders(w.Header(), filterHeaders(resp.Header))
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return resp.StatusCode, err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return resp.StatusCode, nil
		}
		if readErr != nil {
			return resp.StatusCode, readErr
		}
	}
}

func (s *Service) callBackend(ctx context.Context, backend storage.Backend, payload []byte, requestID string) (ProxyResult, usageSummary, error) {
	upstreamURL := backend.BaseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		return ProxyResult{}, usageSummary{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return ProxyResult{}, usageSummary{}, err
	}
	defer resp.Body.Close()

	result := ProxyResult{
		StatusCode: resp.StatusCode,
		Headers:    filterHeaders(resp.Header),
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProxyResult{}, usageSummary{}, err
	}
	result.Body = body
	if resp.StatusCode >= 500 {
		return ProxyResult{}, usageSummary{}, fmt.Errorf("upstream %s returned %d", backend.Name, resp.StatusCode)
	}
	return result, parseUsage(body), nil
}

func (s *Service) buildAttemptOrder(backends []storage.Backend) []storage.Backend {
	rankedBackends := make([]rankedBackend, 0, len(backends))
	for _, backend := range backends {
		if backend.Status == "disabled" {
			continue
		}
		score := backendScore(backend)
		rankedBackends = append(rankedBackends, rankedBackend{backend: backend, score: score})
	}
	if len(rankedBackends) == 0 {
		return nil
	}

	order := make([]storage.Backend, 0, len(rankedBackends))
	for len(rankedBackends) > 0 {
		bestScore := rankedBackends[0].score
		for _, item := range rankedBackends {
			if item.score < bestScore {
				bestScore = item.score
			}
		}
		var group []rankedBackend
		var rest []rankedBackend
		for _, item := range rankedBackends {
			if item.score == bestScore {
				group = append(group, item)
			} else {
				rest = append(rest, item)
			}
		}
		for len(group) > 0 {
			index := s.weightedPick(group)
			order = append(order, group[index].backend)
			group = append(group[:index], group[index+1:]...)
		}
		rankedBackends = rest
	}
	return order
}

func (s *Service) weightedPick(group []rankedBackend) int {
	totalWeight := 0
	for _, item := range group {
		if item.backend.Weight > 0 {
			totalWeight += item.backend.Weight
		}
	}
	if totalWeight <= 0 {
		return 0
	}
	s.mu.Lock()
	choice := s.rng.Intn(totalWeight)
	s.mu.Unlock()
	running := 0
	for index, item := range group {
		weight := item.backend.Weight
		if weight <= 0 {
			weight = 1
		}
		running += weight
		if choice < running {
			return index
		}
	}
	return 0
}

func (s *Service) checkBackend(ctx context.Context, backend storage.Backend) (string, string) {
	timeout := s.cfg.RequestTimeout
	if backend.TimeoutMs > 0 {
		timeout = time.Duration(backend.TimeoutMs) * time.Millisecond
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, backend.BaseURL+backend.HealthcheckPath, nil)
	if err != nil {
		return "unhealthy", err.Error()
	}
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "unhealthy", err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "healthy", ""
	}
	return "unhealthy", fmt.Sprintf("healthcheck returned %d", resp.StatusCode)
}

func backendScore(backend storage.Backend) int {
	statusScore := 0
	switch backend.Status {
	case "active":
		statusScore = 0
	case "draining":
		statusScore = 1
	default:
		statusScore = 2
	}
	healthScore := 0
	switch backend.HealthStatus {
	case "healthy":
		healthScore = 0
	case "unknown":
		healthScore = 1
	default:
		healthScore = 5
	}
	return statusScore*10000 + healthScore*1000 + backend.Priority
}

func filterHeaders(source http.Header) http.Header {
	target := make(http.Header)
	for key, values := range source {
		switch strings.ToLower(key) {
		case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		default:
			for _, value := range values {
				target.Add(key, value)
			}
		}
	}
	return target
}

func copyHeaders(destination http.Header, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func parseUsage(body []byte) usageSummary {
	var envelope struct {
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return usageSummary{}
	}
	return usageSummary{
		PromptTokens:     envelope.Usage.PromptTokens,
		CompletionTokens: envelope.Usage.CompletionTokens,
		TotalTokens:      envelope.Usage.TotalTokens,
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"type":"%s","message":%q}}`, code, message))
}

func randomToken(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	bytes := make([]byte, length)
	if _, err := crand.Read(bytes); err != nil {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for i := range bytes {
			bytes[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(bytes)
	}
	for i := range bytes {
		bytes[i] = alphabet[int(bytes[i])%len(alphabet)]
	}
	return string(bytes)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func apiKeyName(auth *storage.APIKeyAuth) string {
	if auth == nil {
		return ""
	}
	return auth.Name
}
