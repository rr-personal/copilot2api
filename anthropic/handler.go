package anthropic

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/whtsky/copilot2api/debug"
	"github.com/whtsky/copilot2api/internal/models"
	"github.com/whtsky/copilot2api/internal/reqctx"
	"github.com/whtsky/copilot2api/internal/sse"
	"github.com/whtsky/copilot2api/internal/upstream"
	"github.com/whtsky/copilot2api/stats"
)

// Handler handles Anthropic Messages API requests
type Handler struct {
	upstream      *upstream.Client
	models        *models.Cache
	StatsRecorder *stats.Recorder
}

// tokenUsage holds token statistics collected during a request.
type tokenUsage struct {
	In       int // input_tokens (raw, excludes cache)
	Cached   int // cache_read_input_tokens
	NewCache int // cache_creation_input_tokens
	Out      int // output_tokens
}

// accountInfo returns account_id and username log attrs for this handler's
// token provider, or empty strings if unavailable.
func (h *Handler) accountInfo() (accountID, username string) {
	if aip, ok := h.upstream.TokenProvider.(upstream.AccountInfoProvider); ok {
		return aip.GetAccountInfo()
	}
	return "", ""
}

// NewHandler creates a new Anthropic handler.
// The transport is used for upstream HTTP requests (pass nil to create a new one).
func NewHandler(authClient upstream.TokenProvider, transport *http.Transport, mc *models.Cache) *Handler {
	return &Handler{
		upstream: upstream.NewClient(authClient, transport),
		models:   mc,
	}
}

// ServeHTTP handles /v1/messages requests
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != "POST" {
		WriteAnthropicError(w, http.StatusMethodNotAllowed, AnthropicErrorTypeInvalidRequest, "Method not allowed")
		return
	}

	// Handle /v1/messages/count_tokens — estimate token count locally
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		h.handleCountTokens(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, upstream.MaxRequestBody) // 10MB limit
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	// Parse Anthropic request
	var anthropicReq AnthropicMessagesRequest
	if err := json.Unmarshal(reqBody, &anthropicReq); err != nil {
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Validate request
	if err := h.validateRequest(anthropicReq); err != nil {
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid request: %v", err))
		return
	}

	// Preserve the exact model id the client requested. All response paths
	// (native passthrough, Responses, Chat Completions; streaming and not) must
	// echo this back in the response `model` field so clients can match their
	// own config by model id. The real upstream model id is surfaced separately
	// via the X-Upstream-Model response header for debugging.
	originalModel := anthropicReq.Model

	// Resolve model alias (e.g. claude-haiku-4-5-20251001 -> claude-haiku-4.5)
	resolvedModel := resolveModelAlias(anthropicReq.Model)

	// Force 1M context window. GitHub Copilot no longer exposes separate "-1m"
	// model IDs; instead the 1M window is unlocked via the anthropic-beta header
	// "context-1m-2025-08-07" (mirrors the VSCode "Context Size: 1M" toggle). We
	// always force this beta header for capable Claude models so the upstream
	// never falls back to the 200k hard limit. The header is injected on every
	// upstream call via h.context1mHeaders(); see forceContext1M for gating.

	modelChanged := resolvedModel != anthropicReq.Model
	if modelChanged {
		slog.Debug("resolved model alias", "from", anthropicReq.Model, "to", resolvedModel)
		anthropicReq.Model = resolvedModel
	}

	// Debug capture: save request body for configured models
	debug.CaptureRequest(anthropicReq.Model, reqBody)

	route := "chat_completions" // default fallback
	var usage tokenUsage
	reasoningEffort := requestedReasoningEffort(anthropicReq)
	accountID, username := h.accountInfo()
	clientIP := reqctx.GetClientIP(r)
	affinityAccount, affinityHit, hasCache, isGateway := reqctx.GetAffinity(r.Context())
	defer func() {
		logMsg := "anthropic request"
		attrs := []any{
			"endpoint", "/v1/messages",
			"client_ip", clientIP,
			"model", anthropicReq.Model,
			"stream", anthropicReq.Stream,
			"messages", len(anthropicReq.Messages),
			"reasoning_effort", reasoningEffort,
			"route", route,
			"duration_ms", time.Since(start).Milliseconds(),
			"account_id", accountID,
			"username", username,
			"tokens_in_all", usage.In + usage.Cached + usage.NewCache,
			"tokens_in_nocache", usage.In,
			"tokens_cached", usage.Cached,
			"tokens_new_cache", usage.NewCache,
			"tokens_out", usage.Out,
			"tokens_total_all", usage.In + usage.Cached + usage.NewCache + usage.Out,
			"tokens_total_nocache", usage.In + usage.Out,
		}
		if isGateway {
			logMsg = "gateway (anthropic request)"
			attrs = append(attrs, "cache_control", hasCache, "affinity", affinityHit, "affinity_account", affinityAccount)
		}
		slog.Info(logMsg, attrs...)
		if h.StatsRecorder != nil && (usage.In+usage.Cached+usage.NewCache+usage.Out) > 0 {
			h.StatsRecorder.Record(stats.Entry{
				Timestamp:       time.Now(),
				AccountID:       accountID,
				Username:        username,
				Model:           anthropicReq.Model,
				ReasoningEffort: reasoningEffort,
				Endpoint:        "/v1/messages",
				Route:           route,
				TokensIn:        usage.In + usage.Cached + usage.NewCache,
				TokensOut:       usage.Out,
				TokensCached:    usage.Cached,
				TokensNewCache:  usage.NewCache,
				TokensTotal:     usage.In + usage.Cached + usage.NewCache + usage.Out,
				DurationMs:      time.Since(start).Milliseconds(),
			})
		}
	}()

	modelInfo, capabilityFetchFailed := h.getModelInfo(r.Context(), anthropicReq.Model)

	if modelSupportsEndpoint(modelInfo, "/v1/messages") {
		route = "native"
		cacheControlInfo := inspectCacheControl(reqBody)
		topLevelInfo := inspectTopLevelFields(reqBody)
		slog.Debug("native /messages passthrough request", "model", anthropicReq.Model, "top_level_keys", topLevelInfo.Keys, "has_context_management", topLevelInfo.HasContextManagement, "cache_control_count", cacheControlInfo.Count, "cache_control_scope_count", cacheControlInfo.ScopeCount, "cache_control_paths", cacheControlInfo.Paths, "cache_control_scope_paths", cacheControlInfo.ScopePaths)
		// Only re-encode the body for native passthrough (the only path that
		// sends raw reqBody). Responses and Chat Completions paths use the
		// parsed struct, so they skip this JSON round-trip.
		if modelChanged || cacheControlInfo.ScopeCount > 0 || topLevelInfo.HasContextManagement || topLevelInfo.HasEnabledThinking {
			newBody, err := normalizeNativeMessagesBody(reqBody, resolvedModel, modelChanged)
			if err != nil {
				WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid JSON: %v", err))
				return
			}
			if cacheControlInfo.ScopeCount > 0 {
				slog.Debug("normalized native /messages request", "removed_cache_control_scope_paths", cacheControlInfo.ScopePaths)
			}
			if topLevelInfo.HasContextManagement {
				slog.Debug("normalized native /messages request", "removed_top_level_field", "context_management")
			}
			if topLevelInfo.HasEnabledThinking {
				slog.Debug("normalized native /messages request", "rewritten_thinking_type", "enabled -> adaptive")
			}
			reqBody = newBody
		}
		h.handleNativeMessagesPassthrough(w, r, reqBody, anthropicReq.Model, originalModel, anthropicReq.Stream, &usage)
		return
	}

	// Route based on model capabilities
	if modelSupportsEndpoint(modelInfo, "/responses") {
		route = "responses"
		h.handleViaResponsesAPI(w, r, anthropicReq, originalModel, &usage)
		return
	}

	if capabilityFetchFailed {
		slog.Warn("failed to fetch model capabilities, falling back to Chat Completions", "model", anthropicReq.Model)
	}

	h.handleViaChatCompletions(w, r, anthropicReq, originalModel, &usage)
}

func requestedReasoningEffort(req AnthropicMessagesRequest) string {
	var explicit string
	if req.OutputConfig != nil {
		explicit = req.OutputConfig.Effort
	}
	var budget *int
	if req.Thinking != nil {
		budget = req.Thinking.BudgetTokens
	}
	return stats.ClassifyReasoningEffort(explicit, budget)
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

func (h *Handler) validateRequest(req AnthropicMessagesRequest) error {
	if req.Model == "" {
		return fmt.Errorf("model is required")
	}

	if req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens must be positive")
	}

	if len(req.Messages) == 0 && req.System == nil {
		return fmt.Errorf("either messages or system must be provided")
	}

	return nil
}

func (h *Handler) handleNativeMessagesPassthrough(w http.ResponseWriter, r *http.Request, body []byte, model string, originalModel string, stream bool, usage *tokenUsage) {
	// Force the 1M context beta header for capable models, merging with any beta the client already sent.
	beta1m := mergeContext1MBeta(model, r.Header.Get("anthropic-beta"))
	if stream {
		resp, _, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/v1/messages", Body: body, Stream: true, QueryString: r.URL.RawQuery, ExtraHeaders: beta1m})
		if err != nil {
			var upstreamErr *upstream.UpstreamError
			if errors.As(err, &upstreamErr) {
				h.writeRawUpstreamError(w, upstreamErr)
				return
			}
			upstream.LogRequestError("native /messages streaming request failed", err)
			sse.BeginSSE(w)
			w.WriteHeader(http.StatusBadGateway)
			h.writeSSEError(w, "Upstream streaming request failed")
			return
		}
		defer resp.Body.Close()

		flusher, ok := w.(http.Flusher)
		if !ok {
			WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Streaming unsupported")
			return
		}

		reader := bufio.NewReaderSize(resp.Body, 32*1024)
		// Delay sse.BeginSSE until we have inspected the first data line, so we
		// can set the X-Upstream-Model response header (headers must be written
		// before the first body byte). The model id leaks in the message_start
		// event, which is the first SSE event upstream sends.
		sseStarted := false
		beginSSE := func() {
			if !sseStarted {
				sse.BeginSSE(w)
				sseStarted = true
			}
		}
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				// Filter out "data: [DONE]" which is not part of the Anthropic SSE spec
				if strings.TrimSpace(string(line)) == "data: [DONE]" {
					if errors.Is(err, io.EOF) {
						break
					}
					continue
				}
				// Try to extract usage from message_delta / message_stop events
				extractNativeStreamUsage(line, usage)
				// Rewrite the upstream model id in the message_start event back to
				// the client-requested model id, and surface the real upstream
				// model via the X-Upstream-Model header (only possible before the
				// SSE stream has started flushing).
				if rewritten, upstreamModel, changed := rewriteNativeStreamLineModel(line, originalModel); changed {
					if !sseStarted {
						setUpstreamModelHeader(w, upstreamModel)
					}
					line = rewritten
				}
				beginSSE()
				if _, writeErr := w.Write(line); writeErr != nil {
					slog.Error("failed to write native /messages stream", "error", writeErr)
					return
				}
				// Flush at SSE event boundaries (blank lines) instead of every line
				// to reduce syscall overhead while maintaining correct SSE delivery.
				if isBlankSSELine(line) {
					flusher.Flush()
				}
			}

			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				slog.Error("error reading native /messages stream", "error", err)
				return
			}
		}

		// Defensive: if the upstream returned no parseable lines at all, still
		// open the SSE stream so the client sees a well-formed (empty) response.
		beginSSE()

		return
	}

	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/v1/messages", Body: body, QueryString: r.URL.RawQuery, ExtraHeaders: beta1m})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			h.writeRawUpstreamError(w, upstreamErr)
			return
		}
		upstream.LogRequestError("native /messages request failed", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Upstream request failed")
		return
	}

	// Extract usage from non-streaming native response
	extractNativeResponseUsage(respData, usage)

	// Rewrite the upstream model id back to the client-requested model id and
	// surface the real upstream model via the X-Upstream-Model header.
	if rewritten, upstreamModel, changed := rewriteNativeResponseModel(respData, originalModel); changed {
		setUpstreamModelHeader(w, upstreamModel)
		respData = rewritten
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respData)
}

// --- Chat Completions path (existing fallback) ---

func (h *Handler) handleViaChatCompletions(w http.ResponseWriter, r *http.Request, anthropicReq AnthropicMessagesRequest, originalModel string, usage *tokenUsage) {
	openAIReq, err := ConvertAnthropicToOpenAI(anthropicReq)
	if err != nil {
		slog.Error("failed to convert Anthropic request to OpenAI", "error", err)
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Failed to convert request: %v", err))
		return
	}

	if anthropicReq.Stream {
		h.handleStreamingRequest(w, r, openAIReq, originalModel, usage)
	} else {
		h.handleNonStreamingRequest(w, r, openAIReq, originalModel, usage)
	}
}

func (h *Handler) handleNonStreamingRequest(w http.ResponseWriter, r *http.Request, openAIReq OpenAIChatCompletionsRequest, originalModel string, usage *tokenUsage) {
	openAIReq.Stream = false
	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/chat/completions", Body: openAIReq, ExtraHeaders: context1mHeaders(openAIReq.Model)})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			h.handleUpstreamError(w, upstreamErr)
			return
		}
		upstream.LogRequestError("upstream request failed", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Upstream request failed")
		return
	}

	slog.Debug("chat completions response", "size", len(respData))

	var openAIResp OpenAIChatCompletionsResponse
	if err := json.Unmarshal(respData, &openAIResp); err != nil {
		slog.Error("failed to parse OpenAI response", "error", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Failed to parse upstream response")
		return
	}

	anthropicResp, err := ConvertOpenAIToAnthropic(openAIResp)
	if err != nil {
		slog.Error("failed to convert OpenAI response to Anthropic", "error", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Failed to convert response")
		return
	}

	usage.In = anthropicResp.Usage.InputTokens
	usage.Cached = anthropicResp.Usage.CacheReadInputTokens
	usage.NewCache = anthropicResp.Usage.CacheCreationInputTokens
	usage.Out = anthropicResp.Usage.OutputTokens

	// Echo the client-requested model id; expose the real upstream model id via header.
	setUpstreamModelHeader(w, anthropicResp.Model)
	anthropicResp.Model = originalModel

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropicResp)
}

func (h *Handler) handleStreamingRequest(w http.ResponseWriter, r *http.Request, openAIReq OpenAIChatCompletionsRequest, originalModel string, usage *tokenUsage) {
	openAIReq.Stream = true
	resp, _, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/chat/completions", Body: openAIReq, Stream: true, ExtraHeaders: context1mHeaders(openAIReq.Model)})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			h.handleUpstreamError(w, upstreamErr)
			return
		}
		upstream.LogRequestError("upstream streaming request failed", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Upstream streaming request failed")
		return
	}
	defer resp.Body.Close()

	state := NewStreamState()

	finished := h.streamSSE(w, resp.Body, func(event *upstreamSSEEvent) ([]AnthropicStreamEvent, bool, error) {
		var chunk OpenAIChatCompletionChunk
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			slog.Warn("failed to parse OpenAI chunk", "error", err, "data", truncate(event.Data, 200))
			return nil, false, nil
		}

		events, err := ConvertOpenAIChunkToAnthropicEvents(chunk, state)
		if err != nil {
			return nil, false, err
		}

		// Echo the client-requested model id in the message_start event.
		overrideStreamEventModel(events, originalModel)

		// Capture usage from the final chunk (stream usage event)
		if chunk.Usage != nil {
			usage.In = chunk.Usage.PromptTokens
			usage.Out = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokensDetails != nil {
				usage.Cached = chunk.Usage.PromptTokensDetails.CachedTokens
				usage.In -= usage.Cached
				if usage.In < 0 {
					usage.In = 0
				}
			}
		}

		return events, state.Finished, nil
	})

	if !finished {
		slog.Warn("chat completions stream ended without finish event")
	}
}

// --- Responses API path ---

func (h *Handler) handleViaResponsesAPI(w http.ResponseWriter, r *http.Request, anthropicReq AnthropicMessagesRequest, originalModel string, usage *tokenUsage) {
	responsesReq, err := ConvertAnthropicToResponses(anthropicReq)
	if err != nil {
		slog.Error("failed to convert Anthropic request to Responses", "error", err)
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Failed to convert request: %v", err))
		return
	}

	slog.Debug("responses request", "model", responsesReq.Model, "input_items", len(responsesReq.Input), "stream", responsesReq.Stream)

	if anthropicReq.Stream {
		h.handleResponsesStreaming(w, r, responsesReq, originalModel, usage)
	} else {
		h.handleResponsesNonStreaming(w, r, responsesReq, originalModel, usage)
	}
}

func (h *Handler) handleResponsesNonStreaming(w http.ResponseWriter, r *http.Request, responsesReq ResponsesRequest, originalModel string, usage *tokenUsage) {
	responsesReq.Stream = false
	_, respData, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/responses", Body: responsesReq, ExtraHeaders: context1mHeaders(responsesReq.Model)})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			h.handleUpstreamError(w, upstreamErr)
			return
		}
		upstream.LogRequestError("responses upstream request failed", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Upstream request failed")
		return
	}

	slog.Debug("responses result", "size", len(respData))

	var result ResponsesResult
	if err := json.Unmarshal(respData, &result); err != nil {
		slog.Error("failed to parse Responses result", "error", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Failed to parse upstream response")
		return
	}

	anthropicResp := ConvertResponsesToAnthropic(result)
	slog.Debug("translated anthropic response", "id", anthropicResp.ID, "stop_reason", anthropicResp.StopReason, "content_blocks", len(anthropicResp.Content))

	usage.In = anthropicResp.Usage.InputTokens
	usage.Cached = anthropicResp.Usage.CacheReadInputTokens
	usage.NewCache = anthropicResp.Usage.CacheCreationInputTokens
	usage.Out = anthropicResp.Usage.OutputTokens

	// Echo the client-requested model id; expose the real upstream model id via header.
	setUpstreamModelHeader(w, anthropicResp.Model)
	anthropicResp.Model = originalModel

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropicResp)
}

func (h *Handler) handleResponsesStreaming(w http.ResponseWriter, r *http.Request, responsesReq ResponsesRequest, originalModel string, usage *tokenUsage) {
	responsesReq.Stream = true
	resp, _, err := h.upstream.Do(r.Context(), upstream.Request{Endpoint: "/responses", Body: responsesReq, Stream: true, ExtraHeaders: context1mHeaders(responsesReq.Model)})
	if err != nil {
		var upstreamErr *upstream.UpstreamError
		if errors.As(err, &upstreamErr) {
			h.handleUpstreamError(w, upstreamErr)
			return
		}
		upstream.LogRequestError("responses streaming request failed", err)
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Upstream streaming request failed")
		return
	}
	defer resp.Body.Close()

	state := NewResponsesStreamState()

	finished := h.streamSSE(w, resp.Body, func(event *upstreamSSEEvent) ([]AnthropicStreamEvent, bool, error) {
		var streamEvent ResponseStreamEvent
		if err := json.Unmarshal([]byte(event.Data), &streamEvent); err != nil {
			slog.Debug("failed to parse Responses stream event", "error", err, "data", truncate(event.Data, 200), "event", event.Event)
			return nil, false, nil
		}
		if streamEvent.Type == "" && event.Event != "" {
			streamEvent.Type = event.Event
		}

		// Capture usage from response.completed / response.incomplete event
		if (streamEvent.Type == "response.completed" || streamEvent.Type == "response.incomplete") && streamEvent.Response != nil && streamEvent.Response.Usage != nil {
			u := streamEvent.Response.Usage
			usage.In = u.InputTokens
			usage.Out = u.OutputTokens
			if u.InputTokensDetails != nil {
				usage.Cached = u.InputTokensDetails.CachedTokens
				usage.In -= usage.Cached
				if usage.In < 0 {
					usage.In = 0
				}
			}
		}

		events := TranslateResponsesStreamEvent(streamEvent, state)

		// Echo the client-requested model id in the message_start event.
		overrideStreamEventModel(events, originalModel)

		slog.Debug("responses stream event translated", "type", streamEvent.Type, "output_events", len(events))

		return events, state.MessageCompleted, nil
	})

	if finished {
		slog.Debug("responses stream completed")
	} else {
		slog.Warn("responses stream ended without completion")
	}
}

// sseTranslator translates a raw upstream SSE event into Anthropic stream
// events. It returns the translated events, whether the stream is logically
// complete (done=true), and any fatal error that should abort the stream.
// Returning (nil, false, nil) skips the event silently.
type sseTranslator func(event *upstreamSSEEvent) (events []AnthropicStreamEvent, done bool, err error)

// streamSSE is the shared SSE read-translate-write loop used by both the Chat
// Completions and Responses streaming paths. It sets the SSE response headers,
// reads upstream SSE events, passes each one to translate, writes the resulting
// Anthropic events, and flushes. It returns true when the translator signals
// completion, false otherwise (EOF / read error / write error).
func (h *Handler) streamSSE(w http.ResponseWriter, body io.Reader, translate sseTranslator) (finished bool) {
	sse.BeginSSE(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteAnthropicError(w, http.StatusInternalServerError, AnthropicErrorTypeAPI, "Streaming unsupported")
		return false
	}

	reader := bufio.NewReaderSize(body, 32*1024)

	for {
		sseEvent, readErr := readSSEEvent(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			slog.Error("error reading streaming response", "error", readErr)
			h.writeSSEError(w, "Error reading streaming response")
			return false
		}
		if sseEvent == nil {
			continue
		}

		dataStr := strings.TrimSpace(sseEvent.Data)
		if dataStr == "" {
			continue
		}
		if dataStr == "[DONE]" {
			// [DONE] is not part of the Anthropic SSE spec; treat it as
			// a clean stream end (same as translator signalling done).
			return true
		}
		sseEvent.Data = dataStr // pass pre-trimmed data to translator

		events, done, err := translate(sseEvent)
		if err != nil {
			slog.Error("failed to translate streaming event", "error", err)
			h.writeSSEError(w, "Failed to convert streaming response")
			return false
		}

		for _, event := range events {
			if err := h.writeSSEEvent(w, event); err != nil {
				slog.Error("failed to write SSE event", "error", err)
				return false
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}

		if done {
			return true
		}
	}

	// Stream ended without the translator signalling completion.
	h.writeSSEError(w, "Stream ended unexpectedly without completion")
	h.writeSSEEvent(w, AnthropicStreamEvent{Type: "message_stop"})
	flusher.Flush()
	return false
}

type upstreamSSEEvent struct {
	Event string
	Data  string
}

const maxSSELineSize = 1 << 20 // 1MB

// errSSELineTooLong is returned when a single SSE line exceeds maxSSELineSize.
var errSSELineTooLong = fmt.Errorf("SSE line exceeds %d bytes", maxSSELineSize)

func readSSEEvent(reader *bufio.Reader) (*upstreamSSEEvent, error) {
	var (
		eventType string
		dataLines []string
	)

	for {
		line, err := readLimitedLine(reader, maxSSELineSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if eventType == "" && len(dataLines) == 0 {
				if errors.Is(err, io.EOF) {
					return nil, io.EOF
				}
				continue
			}
			return &upstreamSSEEvent{
				Event: eventType,
				Data:  strings.Join(dataLines, "\n"),
			}, nil
		}

		if !strings.HasPrefix(line, ":") {
			field, value, found := strings.Cut(line, ":")
			if !found {
				field = line
				value = ""
			} else {
				value = strings.TrimPrefix(value, " ")
			}

			switch field {
			case "event":
				eventType = value
			case "data":
				dataLines = append(dataLines, value)
			}
		}

		if errors.Is(err, io.EOF) {
			if eventType == "" && len(dataLines) == 0 {
				return nil, io.EOF
			}
			return &upstreamSSEEvent{
				Event: eventType,
				Data:  strings.Join(dataLines, "\n"),
			}, nil
		}
	}
}

// readLimitedLine reads a line (up to and including '\n') from reader.
// It uses ReadSlice for efficiency and falls back to accumulation when
// the line spans multiple buffer fills, returning errSSELineTooLong if
// the accumulated length exceeds maxLen.
func readLimitedLine(reader *bufio.Reader, maxLen int) (string, error) {
	slice, err := reader.ReadSlice('\n')
	if err == nil {
		// Common fast path: full line fit in the buffer.
		if len(slice) > maxLen {
			return "", errSSELineTooLong
		}
		return string(slice), nil
	}
	if err != bufio.ErrBufferFull {
		// io.EOF or other real error — return whatever was read.
		if len(slice) > maxLen {
			return "", errSSELineTooLong
		}
		return string(slice), err
	}
	// Line is longer than the internal buffer — accumulate chunks.
	var buf strings.Builder
	buf.Write(slice)
	for {
		if buf.Len() > maxLen {
			return "", errSSELineTooLong
		}
		slice, err = reader.ReadSlice('\n')
		buf.Write(slice)
		if err != bufio.ErrBufferFull {
			if buf.Len() > maxLen {
				return "", errSSELineTooLong
			}
			return buf.String(), err
		}
	}
}

// --- SSE helpers ---

func (h *Handler) writeSSEEvent(w io.Writer, event AnthropicStreamEvent) error {
	eventData, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, eventData)
	return err
}

func (h *Handler) writeSSEError(w io.Writer, message string) {
	h.writeSSETypedError(w, AnthropicErrorTypeAPI, message)
}

func (h *Handler) writeSSETypedError(w io.Writer, errorType, message string) {
	errorEvent := CreateErrorEvent(message)
	if errorEvent.Error != nil {
		errorEvent.Error.Type = errorType
	}
	h.writeSSEEvent(w, errorEvent)
}

func mapOpenAIErrorTypeToAnthropic(errorType string) string {
	switch errorType {
	case "invalid_request_error":
		return AnthropicErrorTypeInvalidRequest
	case "authentication_error", "invalid_api_key":
		return AnthropicErrorTypeAuthentication
	case "permission_error", "insufficient_quota":
		return AnthropicErrorTypePermission
	case "not_found":
		return AnthropicErrorTypeNotFound
	case "rate_limit_exceeded":
		return AnthropicErrorTypeRateLimit
	case "overloaded":
		return AnthropicErrorTypeOverloaded
	default:
		return AnthropicErrorTypeAPI
	}
}

func mapStatusToAnthropicError(statusCode int) (string, string) {
	switch statusCode {
	case http.StatusBadRequest:
		return AnthropicErrorTypeInvalidRequest, "Invalid request"
	case http.StatusUnauthorized:
		return AnthropicErrorTypeAuthentication, "Authentication failed"
	case http.StatusForbidden:
		return AnthropicErrorTypePermission, "Permission denied"
	case http.StatusNotFound:
		return AnthropicErrorTypeNotFound, "Resource not found"
	case http.StatusTooManyRequests:
		return AnthropicErrorTypeRateLimit, "Rate limit exceeded"
	case http.StatusServiceUnavailable:
		return AnthropicErrorTypeOverloaded, "Service temporarily unavailable"
	default:
		return AnthropicErrorTypeAPI, "Internal error"
	}
}

func (h *Handler) mapUpstreamError(upstreamErr *upstream.UpstreamError) (string, string) {
	var openAIError struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}

	anthropicErrorType, message := mapStatusToAnthropicError(upstreamErr.StatusCode)

	if err := json.Unmarshal(upstreamErr.Body, &openAIError); err == nil && openAIError.Error.Message != "" {
		message = openAIError.Error.Message
		anthropicErrorType = mapOpenAIErrorTypeToAnthropic(openAIError.Error.Type)
	}

	return anthropicErrorType, message
}

func (h *Handler) writeRawUpstreamError(w http.ResponseWriter, upstreamErr *upstream.UpstreamError) {
	body := string(upstreamErr.Body)
	if len(body) > 150 {
		body = body[:150]
	}
	slog.Warn("anthropic upstream error (raw passthrough)", "status", upstreamErr.StatusCode, "body", body)
	upstreamErr.WriteRawError(w)
}

// handleUpstreamError converts upstream OpenAI errors to Anthropic format
func (h *Handler) handleUpstreamError(w http.ResponseWriter, upstreamErr *upstream.UpstreamError) {
	anthropicErrorType, message := h.mapUpstreamError(upstreamErr)
	body := string(upstreamErr.Body)
	if len(body) > 150 {
		body = body[:150]
	}
	slog.Warn("anthropic upstream error", "status", upstreamErr.StatusCode, "type", anthropicErrorType, "message", message, "upstream_body", body)
	WriteAnthropicError(w, upstreamErr.StatusCode, anthropicErrorType, message)
}

// replaceModelInBody replaces the top-level "model" key in a JSON body via a
// targeted round-trip, preserving all other fields unchanged.
func replaceModelInBody(body []byte, newModel string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	modelJSON, _ := json.Marshal(newModel)
	raw["model"] = modelJSON
	return json.Marshal(raw)
}

func normalizeNativeMessagesBody(body []byte, newModel string, replaceModel bool) ([]byte, error) {
	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	obj, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("request body must be a JSON object")
	}

	if replaceModel {
		obj["model"] = newModel
	}

	delete(obj, "context_management")
	stripCacheControlScope(obj)

	// Rewrite thinking.type "enabled" -> "adaptive" for Copilot backend compatibility.
	// Also map budget_tokens to output_config.effort if present.
	if thinkingRaw, ok := obj["thinking"]; ok {
		if thinkingObj, ok := thinkingRaw.(map[string]interface{}); ok {
			if thinkingType, ok := thinkingObj["type"].(string); ok && thinkingType == "enabled" {
				thinkingObj["type"] = "adaptive"
				// Map budget_tokens to output_config.effort
				if budgetTokens, ok := thinkingObj["budget_tokens"]; ok {
					var effort string
					switch v := budgetTokens.(type) {
					case float64:
						if v <= 1000 {
							effort = "low"
						} else if v <= 10000 {
							effort = "medium"
						} else {
							effort = "high"
						}
					default:
						effort = "medium"
					}
					if outputConfig, ok := obj["output_config"].(map[string]interface{}); ok {
						outputConfig["effort"] = effort
					} else {
						obj["output_config"] = map[string]interface{}{"effort": effort}
					}
					// Remove budget_tokens — Copilot's adaptive mode does not accept it
					delete(thinkingObj, "budget_tokens")
				}
				obj["thinking"] = thinkingObj
			}
		}
	}

	return json.Marshal(obj)
}

type topLevelFieldInspection struct {
	Keys                 []string
	HasContextManagement bool
	HasEnabledThinking   bool
}

func inspectTopLevelFields(body []byte) topLevelFieldInspection {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return topLevelFieldInspection{}
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	_, hasContextManagement := raw["context_management"]

	// Check if thinking.type == "enabled" (needs to be rewritten to "adaptive")
	hasEnabledThinking := false
	if thinkingRaw, ok := raw["thinking"]; ok {
		if thinkingObj, ok := thinkingRaw.(map[string]interface{}); ok {
			if thinkingType, ok := thinkingObj["type"].(string); ok && thinkingType == "enabled" {
				hasEnabledThinking = true
			}
		}
	}

	return topLevelFieldInspection{Keys: keys, HasContextManagement: hasContextManagement, HasEnabledThinking: hasEnabledThinking}
}

type cacheControlInspection struct {
	Count      int
	ScopeCount int
	Paths      []string
	ScopePaths []string
}

func inspectCacheControl(body []byte) cacheControlInspection {
	var raw interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return cacheControlInspection{}
	}

	result := cacheControlInspection{}
	inspectCacheControlValue(raw, "$", &result)
	return result
}

func inspectCacheControlValue(v interface{}, path string, result *cacheControlInspection) {
	switch node := v.(type) {
	case map[string]interface{}:
		if cacheControl, ok := node["cache_control"].(map[string]interface{}); ok {
			result.Count++
			result.Paths = append(result.Paths, path+".cache_control")
			if _, hasScope := cacheControl["scope"]; hasScope {
				result.ScopeCount++
				result.ScopePaths = append(result.ScopePaths, path+".cache_control.scope")
			}
		}
		for key, child := range node {
			inspectCacheControlValue(child, path+"."+key, result)
		}
	case []interface{}:
		for i, child := range node {
			inspectCacheControlValue(child, fmt.Sprintf("%s[%d]", path, i), result)
		}
	}
}

func stripCacheControlScope(v interface{}) {
	switch node := v.(type) {
	case map[string]interface{}:
		if cacheControl, ok := node["cache_control"].(map[string]interface{}); ok {
			delete(cacheControl, "scope")
		}
		for _, child := range node {
			stripCacheControlScope(child)
		}
	case []interface{}:
		for _, child := range node {
			stripCacheControlScope(child)
		}
	}
}

// isBlankSSELine reports whether line consists solely of newline characters
// (\r and/or \n). SSE uses blank lines to delimit events; flushing only at
// these boundaries reduces syscall overhead while keeping delivery prompt.
func isBlankSSELine(line []byte) bool {
	for _, b := range line {
		if b != '\r' && b != '\n' {
			return false
		}
	}
	return len(line) > 0
}

// truncate limits a string to maxLen characters for debug logging.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// handleCountTokens estimates token count for a /v1/messages/count_tokens request.
// Since Copilot backend doesn't support this endpoint, we estimate locally using
// JSON byte length / 4 with adjustments for tools and model overhead.
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, upstream.MaxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	// Parse just enough to understand the structure
	var req struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		System   json.RawMessage   `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteAnthropicError(w, http.StatusBadRequest, AnthropicErrorTypeInvalidRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Estimate: JSON bytes / 4 ≈ tokens (rough approximation)
	inputTokens := 0
	for _, msg := range req.Messages {
		inputTokens += len(msg)/4 + 3
	}
	if len(req.System) > 0 {
		inputTokens += len(req.System) / 4
	}
	if len(req.Tools) > 0 {
		for _, tool := range req.Tools {
			inputTokens += len(tool) / 4
		}
		// Tool definition overhead for Claude models
		if strings.HasPrefix(req.Model, "claude") {
			inputTokens += 346
		}
	}

	// Apply model-specific multiplier
	if strings.HasPrefix(req.Model, "claude") {
		inputTokens = int(math.Round(float64(inputTokens) * 1.15))
	}
	if inputTokens < 1 {
		inputTokens = 1
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": inputTokens})
}

// WriteAnthropicError writes an error response in Anthropic API format
func WriteAnthropicError(w http.ResponseWriter, statusCode int, errorType string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := AnthropicErrorResponse{
		Type: "error",
		Error: AnthropicError{
			Type:    errorType,
			Message: message,
		},
	}

	json.NewEncoder(w).Encode(errorResp)
}

// overrideStreamEventModel rewrites the `model` field on a message_start event
// (the only Anthropic stream event that carries a model) to the client-requested
// model id. It mutates the events slice in place. No-op when newModel is empty.
func overrideStreamEventModel(events []AnthropicStreamEvent, newModel string) {
	if newModel == "" {
		return
	}
	for i := range events {
		if events[i].Type == "message_start" && events[i].Message != nil {
			events[i].Message.Model = newModel
		}
	}
}

// rewriteNativeResponseModel rewrites the top-level `model` field of a
// non-streaming native /v1/messages JSON response to the client-requested model
// id. It returns the rewritten bytes, the original (upstream) model id, and
// whether a rewrite occurred. When newModel is empty, the upstream model is
// unparseable, or already equal to newModel, it returns (data, upstreamModel,
// false) and the caller should write the original bytes unchanged.
func rewriteNativeResponseModel(data []byte, newModel string) (out []byte, upstreamModel string, changed bool) {
	if newModel == "" {
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
	if upstreamModel == newModel {
		return data, upstreamModel, false
	}
	newModelRaw, err := json.Marshal(newModel)
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

// rewriteNativeStreamLineModel rewrites the `model` field inside a native
// streaming SSE line carrying a message_start event, so the client sees its
// requested model id. It only touches lines whose JSON data is a message_start
// event with a `message.model` field. It returns the rewritten line (preserving
// the trailing newline), the original (upstream) model id, and whether a rewrite
// occurred. Non-message_start lines and unparseable lines pass through unchanged
// (changed=false).
func rewriteNativeStreamLineModel(line []byte, newModel string) (out []byte, upstreamModel string, changed bool) {
	if newModel == "" {
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
	// Cheap pre-check to avoid JSON parsing on the many delta lines.
	if !strings.Contains(dataStr, "message_start") {
		return line, "", false
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
		return line, "", false
	}
	var eventType string
	if t, ok := event["type"]; ok {
		_ = json.Unmarshal(t, &eventType)
	}
	if eventType != "message_start" {
		return line, "", false
	}
	msgRaw, ok := event["message"]
	if !ok {
		return line, "", false
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return line, "", false
	}
	modelRaw, ok := msg["model"]
	if !ok {
		return line, "", false
	}
	if err := json.Unmarshal(modelRaw, &upstreamModel); err != nil {
		return line, "", false
	}
	if upstreamModel == newModel {
		return line, upstreamModel, false
	}
	newModelRaw, err := json.Marshal(newModel)
	if err != nil {
		return line, upstreamModel, false
	}
	msg["model"] = newModelRaw
	newMsgRaw, err := json.Marshal(msg)
	if err != nil {
		return line, upstreamModel, false
	}
	event["message"] = newMsgRaw
	newData, err := json.Marshal(event)
	if err != nil {
		return line, upstreamModel, false
	}
	// Reconstruct the SSE line, preserving leading "data: " and the original
	// trailing newline(s) so flushing/boundary detection is unaffected.
	newline := ""
	if strings.HasSuffix(s, "\r\n") {
		newline = "\r\n"
	} else if strings.HasSuffix(s, "\n") {
		newline = "\n"
	}
	return []byte("data: " + string(newData) + newline), upstreamModel, true
}

// extractNativeResponseUsage parses token usage from a non-streaming native /v1/messages response.
func extractNativeResponseUsage(data []byte, usage *tokenUsage) {
	var resp struct {
		Usage *AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Usage == nil {
		return
	}
	usage.In = resp.Usage.InputTokens
	usage.Cached = resp.Usage.CacheReadInputTokens
	usage.NewCache = resp.Usage.CacheCreationInputTokens
	usage.Out = resp.Usage.OutputTokens
}

// extractNativeStreamUsage tries to parse token usage from an SSE line in a native streaming response.
// Relevant events: message_start (has input usage) and message_delta (has output_tokens).
func extractNativeStreamUsage(line []byte, usage *tokenUsage) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}

	var event struct {
		Type  string `json:"type"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Message *struct {
			Usage *AnthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}

	switch event.Type {
	case "message_start":
		if event.Message != nil && event.Message.Usage != nil {
			usage.In = event.Message.Usage.InputTokens
			usage.Cached = event.Message.Usage.CacheReadInputTokens
			usage.NewCache = event.Message.Usage.CacheCreationInputTokens
		}
	case "message_delta":
		if event.Usage != nil {
			usage.Out = event.Usage.OutputTokens
		}
	}
}
