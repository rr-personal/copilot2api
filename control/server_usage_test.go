package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/whtsky/copilot2api/stats"
)

func TestHandleUsageQueryFiltersReasoningEffort(t *testing.T) {
	baseDir := t.TempDir()
	recorder := stats.NewRecorder(baseDir)
	timestamp := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	recorder.Record(stats.Entry{Timestamp: timestamp, AccountID: "acc", Model: "model", ReasoningEffort: "low", TokensTotal: 1})
	recorder.Record(stats.Entry{Timestamp: timestamp, AccountID: "acc", Model: "model", ReasoningEffort: "high", TokensTotal: 2})
	recorder.Close()

	server := &Server{statsDir: baseDir}
	request := httptest.NewRequest(http.MethodGet, "/usage?start=2026-02-01&end=2026-02-01&reasoning_effort=HIGH", nil)
	response := httptest.NewRecorder()
	server.handleUsageQuery(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var got []stats.AggregatedEntry
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(response) = %d, want 1: %#v", len(got), got)
	}
	if got[0].ReasoningEffort != "high" || got[0].TokensTotal != 2 {
		t.Fatalf("response entry = %#v, want high effort with 2 tokens", got[0])
	}
}
