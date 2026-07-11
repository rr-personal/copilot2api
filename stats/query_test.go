package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestQueryAggregatesAndFiltersReasoningEffort(t *testing.T) {
	baseDir := t.TempDir()
	dayOne := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	dayTwo := dayOne.AddDate(0, 0, 1)

	entries := []Entry{
		{Timestamp: dayTwo, AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: "minimal", TokensIn: 11, TokensTotal: 11},
		{Timestamp: dayOne, AccountID: "acc-b", Username: "bob", Model: "model-z", ReasoningEffort: "HIGH", TokensIn: 1, TokensOut: 2, TokensTotal: 3},
		{Timestamp: dayOne, AccountID: "acc-a", Username: "alice", Model: "model-z", ReasoningEffort: "low", TokensIn: 2, TokensOut: 2, TokensTotal: 4},
		{Timestamp: dayOne, AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: " XHIGH ", TokensIn: 3, TokensOut: 1, TokensCached: 1, TokensTotal: 4},
		{Timestamp: dayOne, AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: "xhigh", TokensIn: 5, TokensOut: 2, TokensCached: 2, TokensNewCache: 1, TokensTotal: 7},
	}
	for _, entry := range entries {
		appendStatsJSON(t, baseDir, entry, entry)
	}

	legacy := map[string]any{
		"timestamp":    dayOne,
		"account_id":   "acc-a",
		"username":     "alice",
		"model":        "model-a",
		"tokens_in":    7,
		"tokens_total": 7,
	}
	appendStatsJSON(t, baseDir, entries[3], legacy)

	start := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.February, 2, 0, 0, 0, 0, time.UTC)
	got, err := Query(baseDir, "", "", start, end)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	want := []AggregatedEntry{
		{Date: "2026-02-01", AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: "unspecified", TokensIn: 7, TokensTotal: 7, RequestCount: 1},
		{Date: "2026-02-01", AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: "xhigh", TokensIn: 8, TokensOut: 3, TokensCached: 3, TokensNewCache: 1, TokensTotal: 11, RequestCount: 2},
		{Date: "2026-02-01", AccountID: "acc-a", Username: "alice", Model: "model-z", ReasoningEffort: "low", TokensIn: 2, TokensOut: 2, TokensTotal: 4, RequestCount: 1},
		{Date: "2026-02-01", AccountID: "acc-b", Username: "bob", Model: "model-z", ReasoningEffort: "high", TokensIn: 1, TokensOut: 2, TokensTotal: 3, RequestCount: 1},
		{Date: "2026-02-02", AccountID: "acc-a", Username: "alice", Model: "model-a", ReasoningEffort: "minimal", TokensIn: 11, TokensTotal: 11, RequestCount: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Query() = %#v, want %#v", got, want)
	}

	filtered, err := QueryWithReasoningEffort(baseDir, "", "", " XHIGH ", start, end)
	if err != nil {
		t.Fatalf("QueryWithReasoningEffort() error = %v", err)
	}
	if !reflect.DeepEqual(filtered, want[1:2]) {
		t.Fatalf("QueryWithReasoningEffort(xhigh) = %#v, want %#v", filtered, want[1:2])
	}

	legacyOnly, err := QueryWithReasoningEffort(baseDir, "", "", "unspecified", start, end)
	if err != nil {
		t.Fatalf("QueryWithReasoningEffort(unspecified) error = %v", err)
	}
	if !reflect.DeepEqual(legacyOnly, want[:1]) {
		t.Fatalf("QueryWithReasoningEffort(unspecified) = %#v, want %#v", legacyOnly, want[:1])
	}
}

func TestRecorderWritesUnspecifiedReasoningEffort(t *testing.T) {
	baseDir := t.TempDir()
	timestamp := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	recorder := NewRecorder(baseDir)
	recorder.Record(Entry{Timestamp: timestamp, AccountID: "acc", Model: "model"})
	recorder.Close()

	path := filepath.Join(baseDir, "acc", "2026", "2026-02-01_model.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if entry.ReasoningEffort != "unspecified" {
		t.Fatalf("ReasoningEffort = %q, want unspecified", entry.ReasoningEffort)
	}
}

func appendStatsJSON(t *testing.T, baseDir string, pathEntry Entry, value any) {
	t.Helper()
	dir := filepath.Join(baseDir, pathEntry.AccountID, pathEntry.Timestamp.Format("2006"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path := filepath.Join(dir, pathEntry.Timestamp.Format("2006-01-02")+"_"+sanitizeModel(pathEntry.Model)+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		t.Fatalf("Encode() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
