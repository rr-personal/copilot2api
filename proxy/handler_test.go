package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whtsky/copilot2api/internal/copilot"
	"github.com/whtsky/copilot2api/internal/models"
	"github.com/whtsky/copilot2api/internal/upstream"
	"github.com/whtsky/copilot2api/stats"
)

func TestAddCopilotHeaders(t *testing.T) {
	req, err := http.NewRequest("POST", "http://example.com", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	token := "test-token-123"
	copilot.AddHeaders(req, token)

	// Test required headers
	expectedHeaders := map[string]string{
		"Authorization":          "Bearer " + token,
		"User-Agent":             copilot.CopilotUserAgent,
		"Editor-Version":         copilot.EditorVersion,
		"Editor-Plugin-Version":  copilot.EditorPluginVersion,
		"Copilot-Integration-Id": "vscode-chat",
		"Openai-Intent":          "conversation-agent",
		"Content-Type":           "application/json",
		"X-Github-Api-Version":   "2025-04-01",
	}

	for header, expectedValue := range expectedHeaders {
		actualValue := req.Header.Get(header)
		if actualValue != expectedValue {
			t.Errorf("Header %s: expected %q, got %q", header, expectedValue, actualValue)
		}
	}

	// Test that X-Request-Id is present
	requestID := req.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Error("X-Request-Id header should be present")
	}
}

func TestCollectForwardHeaders(t *testing.T) {
	srcReq, err := http.NewRequest("POST", "http://example.com", bytes.NewReader([]byte("test")))
	if err != nil {
		t.Fatalf("Failed to create source request: %v", err)
	}

	srcReq.Header.Set("Content-Type", "application/json")
	srcReq.Header.Set("Accept", "text/event-stream")
	srcReq.Header.Set("Cache-Control", "no-cache")
	srcReq.Header.Set("Accept-Encoding", "gzip, deflate")

	headers := collectForwardHeaders(srcReq)

	expectedHeaders := map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
	}

	for header, expectedValue := range expectedHeaders {
		actualValue, ok := headers[header]
		if !ok {
			t.Errorf("Header %s: expected %q, but not present", header, expectedValue)
		} else if actualValue != expectedValue {
			t.Errorf("Header %s: expected %q, got %q", header, expectedValue, actualValue)
		}
	}

	// Accept-Encoding should NOT be forwarded
	if _, ok := headers["Accept-Encoding"]; ok {
		t.Error("Accept-Encoding should not be forwarded")
	}
}

func TestIsStreamingRequest_Handler(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{
			name:     "streaming request",
			body:     `{"stream": true}`,
			expected: true,
		},
		{
			name:     "non-streaming request",
			body:     `{"stream": false}`,
			expected: false,
		},
		{
			name:     "no stream field",
			body:     `{"messages": []}`,
			expected: false,
		},
		{
			name:     "invalid JSON",
			body:     `{"stream": tr`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStreamingRequest([]byte(tt.body))
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestHandler_ServeHTTP_Routing(t *testing.T) {
	// Fake upstream that records the request path and returns a simple response.
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]string{{"id": "gpt-4"}},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		}
	}))
	defer fakeUpstream.Close()

	tp := &stubTokenProvider{baseURL: fakeUpstream.URL}
	uc := upstream.NewClient(tp, nil)
	handler := &Handler{
		upstream:    uc,
		modelsCache: models.NewCache(func() *upstream.Client { return uc }, 5*time.Minute),
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v1/models, got %d", rec.Code)
	}

	// /v1/chat/completions should route correctly (non-streaming)
	body := `{"model":"gpt-4","messages":[],"stream":false}`
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v1/chat/completions, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_HandleModels(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data": []map[string]string{
				{"id": "gpt-4", "object": "model"},
				{"id": "gpt-3.5-turbo", "object": "model"},
			},
		})
	}))
	defer fakeUpstream.Close()

	tp := &stubTokenProvider{baseURL: fakeUpstream.URL}
	uc := upstream.NewClient(tp, nil)
	handler := &Handler{
		upstream:    uc,
		modelsCache: models.NewCache(func() *upstream.Client { return uc }, 5*time.Minute),
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	data, ok := result["data"].([]interface{})
	if !ok {
		t.Fatal("expected 'data' to be a list")
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(data))
	}
}

func TestHandler_HandlePassthrough(t *testing.T) {
	want := map[string]interface{}{
		"id":      "chatcmpl-abc",
		"object":  "chat.completion",
		"choices": []interface{}{},
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want)
	}))
	defer fakeUpstream.Close()

	tp := &stubTokenProvider{baseURL: fakeUpstream.URL}
	uc := upstream.NewClient(tp, nil)
	handler := &Handler{
		upstream:    uc,
		modelsCache: models.NewCache(func() *upstream.Client { return uc }, 5*time.Minute),
	}

	body := `{"model":"gpt-4","messages":[],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.handlePassthrough(rec, req, "/chat/completions", nil, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if got["id"] != "chatcmpl-abc" {
		t.Fatalf("expected id 'chatcmpl-abc', got %v", got["id"])
	}
	if got["object"] != "chat.completion" {
		t.Fatalf("expected object 'chat.completion', got %v", got["object"])
	}
}

func TestHandler_RecordsReasoningEffort(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-nonstream","object":"response","model":"gpt-5","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2}}`))
			return
		}
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-nonstream","object":"chat.completion","model":"gpt-5","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`))
	}))
	defer fakeUpstream.Close()

	tests := []struct {
		name string
		path string
		body string
		want string
	}{
		{
			name: "non-streaming explicit effort",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5","messages":[],"reasoning_effort":"Minimal","stream":false}`,
			want: "minimal",
		},
		{
			name: "streaming budget fallback",
			path: "/v1/chat/completions",
			body: `{"model":"gpt-5","messages":[],"thinking_budget":16000,"stream":true}`,
			want: "high",
		},
		{
			name: "responses explicit effort",
			path: "/v1/responses",
			body: `{"model":"gpt-5","input":[],"reasoning":{"effort":"none"},"stream":false}`,
			want: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			recorder := stats.NewRecorder(dir)
			handler := &Handler{
				upstream:      upstream.NewClient(&stubTokenProvider{baseURL: fakeUpstream.URL}, nil),
				StatsRecorder: recorder,
			}

			req := httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			recorder.Close()

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.Code, resp.Body.String())
			}
			paths, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != 1 {
				t.Fatalf("stats files = %v, want one", paths)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			var entry stats.Entry
			if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
				t.Fatalf("decode stats entry: %v", err)
			}
			if entry.ReasoningEffort != tt.want {
				t.Errorf("ReasoningEffort = %q, want %q", entry.ReasoningEffort, tt.want)
			}
		})
	}
}

func TestGenerateRequestID(t *testing.T) {
	id1 := copilot.GenerateRequestID()
	id2 := copilot.GenerateRequestID()

	if id1 == "" {
		t.Error("GenerateRequestID should return a non-empty string")
	}

	if !strings.HasPrefix(id1, "req_") {
		t.Errorf("GenerateRequestID should start with 'req_', got %s", id1)
	}

	if id1 == id2 {
		t.Error("GenerateRequestID should generate unique IDs")
	}
}

func TestUpstreamError(t *testing.T) {
	err := &upstream.UpstreamError{
		StatusCode: 429,
		Body:       []byte(`{"error": "rate limited"}`),
	}

	expected := "upstream error: status 429, body: {\"error\": \"rate limited\"}"
	if err.Error() != expected {
		t.Errorf("Expected %q, got %q", expected, err.Error())
	}
}

func TestHandleEmbeddingsStringInput(t *testing.T) {
	body := `{"model":"text-embedding-3-small","input":"hello"}`
	var reqMap map[string]json.RawMessage
	json.Unmarshal([]byte(body), &reqMap)

	// String input should be wrappable to array
	var s string
	if err := json.Unmarshal(reqMap["input"], &s); err != nil {
		t.Fatalf("input should unmarshal as string: %v", err)
	}
	wrapped, _ := json.Marshal([]string{s})
	reqMap["input"] = wrapped

	var result []string
	json.Unmarshal(reqMap["input"], &result)
	if len(result) != 1 || result[0] != "hello" {
		t.Errorf("expected [hello], got %v", result)
	}
}

func TestHandleEmbeddingsArrayInput(t *testing.T) {
	body := `{"model":"text-embedding-3-small","input":["hello","world"]}`
	var reqMap map[string]json.RawMessage
	json.Unmarshal([]byte(body), &reqMap)

	// Array input should NOT unmarshal as string
	var s string
	if json.Unmarshal(reqMap["input"], &s) == nil {
		t.Error("array input should not unmarshal as string")
	}

	var result []string
	if err := json.Unmarshal(reqMap["input"], &result); err != nil {
		t.Fatalf("array input should unmarshal as []string: %v", err)
	}
	if len(result) != 2 || result[0] != "hello" || result[1] != "world" {
		t.Errorf("expected [hello world], got %v", result)
	}
}

// stubTokenProvider implements upstream.TokenProvider for tests.
type stubTokenProvider struct {
	baseURL string
}

func (s *stubTokenProvider) GetToken(_ context.Context) (string, error) {
	return "test-token", nil
}

func (s *stubTokenProvider) GetBaseURL() string {
	return s.baseURL
}

// TestHandlePassthrough_StreamingNetworkFailure_Returns502 verifies that when
// the upstream connection fails before any response headers are written (e.g.
// connection refused), the client receives a 502 instead of an empty 200.
func TestHandlePassthrough_StreamingNetworkFailure_Returns502(t *testing.T) {
	// Start a listener and immediately close it to get a guaranteed-refused port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tp := &stubTokenProvider{baseURL: "http://" + addr}
	handler := &Handler{
		upstream: upstream.NewClient(tp, nil),
	}

	body := `{"model":"gpt-4","messages":[],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.handlePassthrough(rec, req, "/chat/completions", nil, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var errResp OpenAIErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Type != OpenAIErrorTypeServerError {
		t.Fatalf("expected error type %q, got %q", OpenAIErrorTypeServerError, errResp.Error.Type)
	}
}
