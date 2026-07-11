package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/debug"
	"github.com/whtsky/copilot2api/internal/models"
	"github.com/whtsky/copilot2api/internal/reqctx"
	"github.com/whtsky/copilot2api/internal/sse"
	"github.com/whtsky/copilot2api/internal/types"
	"github.com/whtsky/copilot2api/internal/upstream"
	"github.com/whtsky/copilot2api/stats"
)

// UsageProvider can return usage information for the /usage endpoint.
type UsageProvider interface {
	GetUsageInfo(ctx context.Context) (interface{}, error)
}

type Handler struct {
	upstream      *upstream.Client
	usageProvider UsageProvider
	modelsCache   *models.Cache
	StatsRecorder *stats.Recorder
}

// NewHandler creates a new proxy handler.
// The transport is used for upstream HTTP requests (pass nil to create a new one).
// usageProvider may be nil if /usage is not needed.
func NewHandler(tokenProvider upstream.TokenProvider, transport *http.Transport, mc *models.Cache, usageProvider UsageProvider) *Handler {
	return &Handler{
		upstream:      upstream.NewClient(tokenProvider, transport),
		usageProvider: usageProvider,
		modelsCache:   mc,
	}
}

// proxyTokenUsage holds token statistics for a proxy request.
type proxyTokenUsage struct {
	In              int    // prompt_tokens (includes cached)
	Cached          int    // prompt_tokens_details.cached_tokens
	Out             int    // completion_tokens
	Total           int    // total_tokens
	ReasoningEffort string // normalized client-requested reasoning effort
}

// accountInfo returns account_id and username for this handler's token provider.
func (h *Handler) accountInfo() (accountID, username string) {
	if aip, ok := h.upstream.TokenProvider.(upstream.AccountInfoProvider); ok {
		return aip.GetAccountInfo()
	}
	return "", ""
}

// ServeHTTP handles all proxy requests
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Extract endpoint from path
	endpoint := strings.TrimPrefix(r.URL.Path, "/v1")
	var usage proxyTokenUsage
	var requestModel string
	accountID, username := h.accountInfo()
	clientIP := reqctx.GetClientIP(r)
	affinityAccount, affinityHit, _, isGateway := reqctx.GetAffinity(r.Context())
	defer func() {
		tokensInNocache := usage.In - usage.Cached
		logMsg := "proxy request"
		attrs := []any{
			"method", r.Method,
			"endpoint", endpoint,
			"client_ip", clientIP,
			"duration_ms", time.Since(start).Milliseconds(),
			"account_id", accountID,
			"username", username,
			"tokens_in_all", usage.In,
			"tokens_in_nocache", tokensInNocache,
			"tokens_cached", usage.Cached,
			"tokens_new_cache", 0,
			"tokens_out", usage.Out,
			"tokens_total_all", usage.Total,
			"tokens_total_nocache", tokensInNocache + usage.Out,
		}
		if isGateway {
			logMsg = "gateway (proxy request)"
			attrs = append(attrs, "affinity", affinityHit, "affinity_account", affinityAccount)
		}
		slog.Info(logMsg, attrs...)
		if h.StatsRecorder != nil && usage.Total > 0 {
			h.StatsRecorder.Record(stats.Entry{
				Timestamp:       time.Now(),
				AccountID:       accountID,
				Username:        username,
				Model:           requestModel,
				Endpoint:        endpoint,
				Route:           "proxy",
				ReasoningEffort: usage.ReasoningEffort,
				TokensIn:        usage.In,
				TokensOut:       usage.Out,
				TokensCached:    usage.Cached,
				TokensTotal:     usage.Total,
				DurationMs:      time.Since(start).Milliseconds(),
			})
		}
	}()

	switch endpoint {
	case "/models":
		h.handleModels(w, r)
	case "/embeddings":
		h.handleEmbeddings(w, r, &usage)
	case "/chat/completions":
		h.handlePassthrough(w, r, endpoint, &usage, &requestModel)
	case "/responses":
		h.handlePassthrough(w, r, endpoint, &usage, &requestModel)
	default:
		WriteOpenAIError(w, http.StatusNotFound, OpenAIErrorTypeInvalidRequest, "Endpoint not found")
	}
}

// handleModels handles /v1/models with caching
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, OpenAIErrorTypeInvalidRequest, "Method not allowed")
		return
	}

	respData, err := h.modelsCache.GetRaw(r.Context())
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			upstreamErr.WriteRawError(w)
			return
		}
		upstream.LogRequestError("failed to fetch models", err)
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Internal server error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respData)
}

// handlePassthrough handles direct passthrough requests
func (h *Handler) handlePassthrough(w http.ResponseWriter, r *http.Request, endpoint string, usage *proxyTokenUsage, modelOut *string) {
	// Check body size before processing — reject oversized payloads with 413
	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(r.Body, upstream.MaxRequestBody+1))
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Failed to read request body")
			return
		}
		if len(bodyBytes) > upstream.MaxRequestBody {
			WriteOpenAIError(w, http.StatusRequestEntityTooLarge, OpenAIErrorTypeInvalidRequest, "Request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	if modelOut != nil && len(bodyBytes) > 0 {
		*modelOut = extractModelField(bodyBytes)
	}
	if usage != nil {
		usage.ReasoningEffort = extractReasoningEffort(endpoint, bodyBytes)
	}

	// Debug capture: save request body for configured models
	if modelOut != nil && *modelOut != "" {
		debug.CaptureRequest(*modelOut, bodyBytes)
	}

	h.handlePassthroughBody(w, r, endpoint, bodyBytes, usage)
}

// handlePassthroughBody processes the passthrough request after the body has been read and validated.
// It takes pre-read body bytes to avoid redundant body reading.
func (h *Handler) handlePassthroughBody(w http.ResponseWriter, r *http.Request, endpoint string, bodyBytes []byte, usage *proxyTokenUsage) {
	// Determine smart routing: should we convert to a different endpoint?
	targetEndpoint := h.resolveTargetEndpoint(r, endpoint, bodyBytes)

	if targetEndpoint != endpoint {
		slog.Info("smart routing: converting request", "from", endpoint, "to", targetEndpoint)
	}

	streaming := isStreamingRequest(bodyBytes)

	// If no conversion needed, passthrough as before
	if targetEndpoint == endpoint {
		if streaming {
			if err := h.HandleStreamingRequest(w, r, endpoint, extractModelField(bodyBytes), usage); err != nil {
				var hse *headersSentError
				if errors.As(err, &hse) {
					// Client disconnected mid-stream — expected, not an error
					slog.Debug("streaming request failed", "error", err, "endpoint", endpoint)
				} else {
					upstream.LogRequestError("streaming request failed", err, "endpoint", endpoint)
					WriteOpenAIError(w, http.StatusBadGateway, OpenAIErrorTypeServerError, "upstream request failed")
				}
			}
			return
		}

		respData, err := h.doNonStreamingRequest(r, endpoint)
		if err != nil {
			var upstreamErr *upstream.UpstreamError
			if errors.As(err, &upstreamErr) {
				upstreamErr.WriteRawError(w)
				return
			}
			upstream.LogRequestError("passthrough request failed", err, "endpoint", endpoint)
			WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Internal server error")
			return
		}

		// Extract usage from non-streaming response
		if endpoint == "/responses" {
			extractUsageFromResponsesResponse(respData, usage)
		} else {
			extractUsageFromChatResponse(respData, usage)
		}

		// Echo the client-requested model id in the response body; expose the
		// real upstream model id via the X-Upstream-Model header.
		if rewritten, upstreamModel, changed := rewriteTopLevelModel(respData, extractModelField(bodyBytes)); changed {
			setUpstreamModelHeader(w, upstreamModel)
			respData = rewritten
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(respData)
		return
	}

	// Conversion needed: route based on direction
	switch {
	case endpoint == "/chat/completions" && targetEndpoint == "/responses":
		if streaming {
			h.handleChatToResponsesStreaming(w, r, bodyBytes, usage)
		} else {
			h.handleChatToResponsesNonStreaming(w, r, bodyBytes, usage)
		}
	case endpoint == "/responses" && targetEndpoint == "/chat/completions":
		if streaming {
			h.handleResponsesToChatStreaming(w, r, bodyBytes, usage)
		} else {
			h.handleResponsesToChatNonStreaming(w, r, bodyBytes, usage)
		}
	}
}

// resolveTargetEndpoint determines which upstream endpoint to use based on
// model capabilities. Returns the original endpoint when no conversion is
// needed (model supports it, model is unknown, or capabilities are unavailable).
func (h *Handler) resolveTargetEndpoint(r *http.Request, endpoint string, bodyBytes []byte) string {
	if h.modelsCache == nil {
		return endpoint
	}

	modelID := extractModelField(bodyBytes)
	if modelID == "" {
		return endpoint // no model field → passthrough
	}

	modelMap, err := h.modelsCache.GetInfo(r.Context())
	if err != nil {
		slog.Debug("smart routing: failed to get model info, falling back to passthrough", "error", err)
		return endpoint
	}

	info := modelMap[modelID]
	// Unknown model → passthrough (best effort)
	if info == nil {
		return endpoint
	}

	// Build preference order: requested endpoint first, then the alternative
	var preferred []string
	switch endpoint {
	case "/chat/completions":
		preferred = []string{"/chat/completions", "/responses"}
	case "/responses":
		preferred = []string{"/responses", "/chat/completions"}
	default:
		return endpoint
	}

	target := models.PickEndpoint(info, preferred)
	if target == "" {
		// Model supports neither → passthrough, let upstream return the error
		return endpoint
	}
	return target
}

// extractModelField extracts the "model" field from a JSON request body.
func extractModelField(body []byte) string {
	var top struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	return top.Model
}

// extractReasoningEffort returns the normalized effort requested by the
// client. Chat Completions' explicit effort takes precedence over its legacy
// thinking budget; Responses requests carry effort under reasoning.effort.
func extractReasoningEffort(endpoint string, body []byte) string {
	switch endpoint {
	case "/chat/completions":
		var req struct {
			ReasoningEffort string `json:"reasoning_effort"`
			ThinkingBudget  *int   `json:"thinking_budget"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return stats.ClassifyReasoningEffort("", nil)
		}
		return stats.ClassifyReasoningEffort(req.ReasoningEffort, req.ThinkingBudget)
	case "/responses":
		var req struct {
			Reasoning *struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Reasoning == nil {
			return stats.ClassifyReasoningEffort("", nil)
		}
		return stats.ClassifyReasoningEffort(req.Reasoning.Effort, nil)
	default:
		return stats.ClassifyReasoningEffort("", nil)
	}
}

// setUpstreamModelHeader records the real upstream model id on a non-standard
// response header so clients/operators can see what the upstream actually
// returned, even though the response `model` field is rewritten to the
// client-requested id. No-op when upstreamModel is empty.
func setUpstreamModelHeader(w http.ResponseWriter, upstreamModel string) {
	if upstreamModel != "" {
		w.Header().Set("X-Upstream-Model", upstreamModel)
	}
}

// upstreamModelIfDifferent returns upstream when it is non-empty and differs
// from requested, otherwise "". Used to only emit the X-Upstream-Model header
// when the upstream actually expanded/changed the model id.
func upstreamModelIfDifferent(upstream, requested string) string {
	if upstream != "" && upstream != requested {
		return upstream
	}
	return ""
}

// rewriteTopLevelModel rewrites the top-level `model` field of a JSON response
// body to the client-requested model id. It returns the rewritten bytes, the
// original (upstream) model id, and whether a rewrite occurred. When
// requestedModel is empty, the body is not a JSON object, the model field is
// missing/unparseable, or already equal to requestedModel, it returns
// (data, upstreamModel, false) and the caller should write the original bytes.
func rewriteTopLevelModel(data []byte, requestedModel string) (out []byte, upstreamModel string, changed bool) {
	if requestedModel == "" {
		return data, "", false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return data, "", false
	}
	modelRaw, ok := raw["model"]
	if !ok {
		return data, "", false
	}
	if err := json.Unmarshal(modelRaw, &upstreamModel); err != nil {
		return data, "", false
	}
	if upstreamModel == requestedModel {
		return data, upstreamModel, false
	}
	newModelRaw, err := json.Marshal(requestedModel)
	if err != nil {
		return data, upstreamModel, false
	}
	raw["model"] = newModelRaw
	out, err = json.Marshal(raw)
	if err != nil {
		return data, upstreamModel, false
	}
	return out, upstreamModel, true
}

// rewriteSSELineModel rewrites the top-level `model` field inside a single SSE
// data line (OpenAI Chat Completions chunks and Responses stream events both
// carry a `model` at the top level of their JSON payload). It returns the
// rewritten line (preserving the trailing newline), the upstream model id seen
// (if any), and whether a rewrite occurred. Lines without a JSON object payload,
// without a `model` field, or already matching requestedModel pass through
// unchanged (changed=false).
func rewriteSSELineModel(line []byte, requestedModel string) (out []byte, upstreamModel string, changed bool) {
	if requestedModel == "" {
		return line, "", false
	}
	s := string(line)
	trimmed := strings.TrimSpace(s)
	const prefix = "data:"
	if !strings.HasPrefix(trimmed, prefix) {
		return line, "", false
	}
	dataStr := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	if dataStr == "" || dataStr == "[DONE]" {
		return line, "", false
	}
	// Cheap pre-check to skip lines that obviously have no model field.
	if !strings.Contains(dataStr, "\"model\"") {
		return line, "", false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dataStr), &raw); err != nil {
		return line, "", false
	}
	modelRaw, ok := raw["model"]
	if !ok {
		return line, "", false
	}
	if err := json.Unmarshal(modelRaw, &upstreamModel); err != nil {
		return line, "", false
	}
	if upstreamModel == "" || upstreamModel == requestedModel {
		return line, upstreamModel, false
	}
	newModelRaw, err := json.Marshal(requestedModel)
	if err != nil {
		return line, upstreamModel, false
	}
	raw["model"] = newModelRaw
	newData, err := json.Marshal(raw)
	if err != nil {
		return line, upstreamModel, false
	}
	newline := ""
	if strings.HasSuffix(s, "\r\n") {
		newline = "\r\n"
	} else if strings.HasSuffix(s, "\n") {
		newline = "\n"
	}
	return []byte("data: " + string(newData) + newline), upstreamModel, true
}

// extractUsageFromChatResponse parses usage fields from an OpenAI Chat Completions JSON response.
func extractUsageFromChatResponse(data []byte, usage *proxyTokenUsage) {
	if usage == nil {
		return
	}
	var resp types.OpenAIChatCompletionsResponse
	if err := json.Unmarshal(data, &resp); err != nil || resp.Usage == nil {
		return
	}
	usage.In = resp.Usage.PromptTokens
	usage.Out = resp.Usage.CompletionTokens
	usage.Total = resp.Usage.TotalTokens
	if resp.Usage.PromptTokensDetails != nil {
		usage.Cached = resp.Usage.PromptTokensDetails.CachedTokens
	}
}

// extractUsageFromResponsesResponse parses usage fields from an OpenAI Responses API JSON response.
func extractUsageFromResponsesResponse(data []byte, usage *proxyTokenUsage) {
	if usage == nil {
		return
	}
	var resp types.ResponsesResult
	if err := json.Unmarshal(data, &resp); err != nil || resp.Usage == nil {
		return
	}
	usage.In = resp.Usage.InputTokens
	usage.Out = resp.Usage.OutputTokens
	usage.Total = resp.Usage.InputTokens + resp.Usage.OutputTokens
	if resp.Usage.InputTokensDetails != nil {
		usage.Cached = resp.Usage.InputTokensDetails.CachedTokens
	}
}

// --- Non-streaming conversion handlers ---

// handleChatToResponsesNonStreaming converts a Chat Completions request to
// Responses API, sends it upstream, and converts the response back.
func (h *Handler) handleChatToResponsesNonStreaming(w http.ResponseWriter, r *http.Request, bodyBytes []byte, usage *proxyTokenUsage) {
	var chatReq types.OpenAIChatCompletionsRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Invalid JSON in request body")
		return
	}

	responsesReq := ConvertChatToResponsesRequest(chatReq)
	reqBody, err := json.Marshal(responsesReq)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal converted request")
		return
	}

	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{
		Method:       r.Method,
		Endpoint:     "/responses",
		Body:         reqBody,
		QueryString:  r.URL.RawQuery,
		ExtraHeaders: collectForwardHeaders(r),
	})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			upstreamErr.WriteRawError(w)
			return
		}
		upstream.LogRequestError("converted request failed", err, "from", "/chat/completions", "to", "/responses")
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Internal server error")
		return
	}

	var responsesResult types.ResponsesResult
	if err := json.Unmarshal(respData, &responsesResult); err != nil {
		slog.Error("failed to parse responses result for conversion", "error", err)
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to parse upstream response")
		return
	}

	// Check for failed response
	if responsesResult.Status == "failed" || responsesResult.Error != nil {
		msg := "Upstream request failed"
		if responsesResult.Error != nil && responsesResult.Error.Message != "" {
			msg = responsesResult.Error.Message
		}
		WriteOpenAIError(w, http.StatusBadGateway, OpenAIErrorTypeServerError, msg)
		return
	}

	chatResp := ConvertResponsesResultToChatResponse(responsesResult, chatReq.Model)
	// Extract usage from converted chat response
	if chatResp.Usage != nil && usage != nil {
		usage.In = chatResp.Usage.PromptTokens
		usage.Out = chatResp.Usage.CompletionTokens
		usage.Total = chatResp.Usage.TotalTokens
		if chatResp.Usage.PromptTokensDetails != nil {
			usage.Cached = chatResp.Usage.PromptTokensDetails.CachedTokens
		}
	}
	result, err := json.Marshal(chatResp)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal response")
		return
	}

	// chatResp.Model is already the client-requested model (chatReq.Model);
	// surface the real upstream model id via the X-Upstream-Model header.
	setUpstreamModelHeader(w, upstreamModelIfDifferent(responsesResult.Model, chatReq.Model))

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// handleResponsesToChatNonStreaming converts a Responses API request to
// Chat Completions, sends it upstream, and converts the response back.
func (h *Handler) handleResponsesToChatNonStreaming(w http.ResponseWriter, r *http.Request, bodyBytes []byte, usage *proxyTokenUsage) {
	var responsesReq types.ResponsesRequest
	if err := json.Unmarshal(bodyBytes, &responsesReq); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Invalid JSON in request body")
		return
	}

	chatReq := ConvertResponsesToChatRequest(responsesReq)
	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal converted request")
		return
	}

	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{
		Method:       r.Method,
		Endpoint:     "/chat/completions",
		Body:         reqBody,
		QueryString:  r.URL.RawQuery,
		ExtraHeaders: collectForwardHeaders(r),
	})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			upstreamErr.WriteRawError(w)
			return
		}
		upstream.LogRequestError("converted request failed", err, "from", "/responses", "to", "/chat/completions")
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Internal server error")
		return
	}

	var chatResp types.OpenAIChatCompletionsResponse
	if err := json.Unmarshal(respData, &chatResp); err != nil {
		slog.Error("failed to parse chat response for conversion", "error", err)
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to parse upstream response")
		return
	}

	responsesResult := ConvertChatResponseToResponsesResult(chatResp)
	// Echo the client-requested model id; expose the real upstream model id via header.
	if responsesReq.Model != "" {
		setUpstreamModelHeader(w, upstreamModelIfDifferent(responsesResult.Model, responsesReq.Model))
		responsesResult.Model = responsesReq.Model
	}
	// Extract usage from chat response
	if chatResp.Usage != nil && usage != nil {
		usage.In = chatResp.Usage.PromptTokens
		usage.Out = chatResp.Usage.CompletionTokens
		usage.Total = chatResp.Usage.TotalTokens
		if chatResp.Usage.PromptTokensDetails != nil {
			usage.Cached = chatResp.Usage.PromptTokensDetails.CachedTokens
		}
	}
	result, err := json.Marshal(responsesResult)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

// doNonStreamingRequest makes a non-streaming request to the Copilot API via the shared upstream client.
func (h *Handler) doNonStreamingRequest(r *http.Request, endpoint string) ([]byte, error) {
	var body interface{}
	if r.Body != nil {
		body = r.Body
	}

	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{
		Method:       r.Method,
		Endpoint:     endpoint,
		Body:         body,
		QueryString:  r.URL.RawQuery,
		ExtraHeaders: collectForwardHeaders(r),
	})
	return respData, err
}

// collectForwardHeaders returns headers from the original request that should be
// forwarded to the upstream API.
func collectForwardHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string)
	for _, name := range []string{"Content-Type", "Accept", "Cache-Control"} {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	return headers
}

// handleUsage returns usage/quota info from the Copilot token response
func (h *Handler) HandleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, OpenAIErrorTypeInvalidRequest, "Method not allowed")
		return
	}

	if h.usageProvider == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, OpenAIErrorTypeServerError, "Usage info not available")
		return
	}
	usage, err := h.usageProvider.GetUsageInfo(r.Context())
	if err != nil {
		slog.Error("failed to get usage info", "error", err)
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to get usage info")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(usage)
}

// handleEmbeddings normalizes input to array format before proxying
func (h *Handler) handleEmbeddings(w http.ResponseWriter, r *http.Request, usage *proxyTokenUsage) {
	r.Body = http.MaxBytesReader(w, r.Body, upstream.MaxRequestBody) // 10MB limit
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Failed to read request body")
		return
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Invalid JSON")
		return
	}

	// If input is a string, wrap it in an array
	if input, ok := req["input"]; ok {
		var s string
		if json.Unmarshal(input, &s) == nil {
			wrapped, _ := json.Marshal([]string{s})
			req["input"] = wrapped
			body, _ = json.Marshal(req)
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.handlePassthroughBody(w, r, "/embeddings", body, usage)
}

// --- Streaming conversion handlers ---

// handleChatToResponsesStreaming sends a converted Chat→Responses request upstream
// and converts the Responses stream events back to Chat Completions chunks.
func (h *Handler) handleChatToResponsesStreaming(w http.ResponseWriter, r *http.Request, bodyBytes []byte, usage *proxyTokenUsage) {
	var chatReq types.OpenAIChatCompletionsRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Invalid JSON in request body")
		return
	}

	responsesReq := ConvertChatToResponsesRequest(chatReq)
	reqBody, err := json.Marshal(responsesReq)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal converted request")
		return
	}

	resp, _, err := h.upstream.Do(r.Context(), upstream.Request{
		Method:       r.Method,
		Endpoint:     "/responses",
		Body:         reqBody,
		QueryString:  r.URL.RawQuery,
		Stream:       true,
		ExtraHeaders: collectForwardHeaders(r),
	})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			upstreamErr.WriteRawError(w)
			return
		}
		upstream.LogRequestError("converted streaming request failed", err, "from", "/chat/completions", "to", "/responses")
		WriteOpenAIError(w, http.StatusBadGateway, OpenAIErrorTypeServerError, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	sse.BeginSSE(w)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	if err := streamResponsesAsChatChunks(w, resp.Body, chatReq.Model, usage); err != nil {
		slog.Error("streaming conversion failed (responses→chat)", "error", err)
		// Headers already sent, can't write HTTP error
	}
}

// handleResponsesToChatStreaming sends a converted Responses→Chat request upstream
// and converts the Chat Completions stream chunks back to Responses API events.
func (h *Handler) handleResponsesToChatStreaming(w http.ResponseWriter, r *http.Request, bodyBytes []byte, usage *proxyTokenUsage) {
	var responsesReq types.ResponsesRequest
	if err := json.Unmarshal(bodyBytes, &responsesReq); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, OpenAIErrorTypeInvalidRequest, "Invalid JSON in request body")
		return
	}

	chatReq := ConvertResponsesToChatRequest(responsesReq)
	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, OpenAIErrorTypeServerError, "Failed to marshal converted request")
		return
	}

	resp, _, err := h.upstream.Do(r.Context(), upstream.Request{
		Method:       r.Method,
		Endpoint:     "/chat/completions",
		Body:         reqBody,
		QueryString:  r.URL.RawQuery,
		Stream:       true,
		ExtraHeaders: collectForwardHeaders(r),
	})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			upstreamErr.WriteRawError(w)
			return
		}
		upstream.LogRequestError("converted streaming request failed", err, "from", "/responses", "to", "/chat/completions")
		WriteOpenAIError(w, http.StatusBadGateway, OpenAIErrorTypeServerError, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	sse.BeginSSE(w)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	if err := streamChatChunksAsResponsesEvents(w, resp.Body, responsesReq.Model, usage); err != nil {
		slog.Error("streaming conversion failed (chat→responses)", "error", err)
		// Headers already sent, can't write HTTP error
	}
}

// --- OpenAI error response helpers ---

// OpenAIErrorResponse represents an error response in OpenAI API format
type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error"`
}

// OpenAIError represents the error object in OpenAI API responses
type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// Error type constants for OpenAI API
const (
	OpenAIErrorTypeServerError    = "server_error"
	OpenAIErrorTypeInvalidRequest = "invalid_request_error"
)

// WriteOpenAIError writes an error response in OpenAI API format
func WriteOpenAIError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := OpenAIErrorResponse{
		Error: OpenAIError{
			Message: message,
			Type:    errorType,
		},
	}

	json.NewEncoder(w).Encode(errorResp)
}
