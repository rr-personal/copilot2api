package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whtsky/copilot2api/auth"
	"github.com/whtsky/copilot2api/stats"
	"github.com/whtsky/copilot2api/storage"
)

func TestHandlerRecordsReasoningEffort(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"gpt-5",
			"choices":[],
			"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}
		}`))
	}))
	defer fakeUpstream.Close()

	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copilotToken, err := json.Marshal(auth.CopilotToken{
		Token:     "test-token",
		ExpiresAt: time.Now().Add(time.Hour),
		BaseURL:   fakeUpstream.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SaveCredentials(context.Background(), "acc-gateway", &storage.Credentials{
		GitHubUsername: "bob",
		CopilotToken:   copilotToken,
	}); err != nil {
		t.Fatal(err)
	}
	accountManager, err := auth.NewAccountManager(backend)
	if err != nil {
		t.Fatal(err)
	}

	statsDir := t.TempDir()
	recorder := stats.NewRecorder(statsDir)
	handler := NewHandler(accountManager, nil, nil)
	handler.Recorder = recorder
	request := httptest.NewRequest(
		http.MethodPost,
		"/gw/api/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[],"reasoning_effort":"XHIGH","stream":false}`),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	recorder.Close()

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	paths, err := filepath.Glob(filepath.Join(statsDir, "acc-gateway", "*", "*.jsonl"))
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
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("decode stats entry: %v", err)
	}
	if entry.ReasoningEffort != "xhigh" {
		t.Errorf("ReasoningEffort = %q, want xhigh", entry.ReasoningEffort)
	}
	if entry.AccountID != "acc-gateway" || entry.Username != "bob" {
		t.Errorf("account = (%q, %q), want (acc-gateway, bob)", entry.AccountID, entry.Username)
	}
	if entry.Endpoint != "/chat/completions" || entry.TokensTotal != 10 {
		t.Errorf("record = endpoint %q, total %d; want /chat/completions, 10", entry.Endpoint, entry.TokensTotal)
	}
}
