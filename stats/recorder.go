package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry represents a single usage record.
type Entry struct {
	Timestamp       time.Time `json:"timestamp"`
	AccountID       string    `json:"account_id"`
	Username        string    `json:"username"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	Endpoint        string    `json:"endpoint"`
	Route           string    `json:"route"`
	TokensIn        int       `json:"tokens_in"`
	TokensOut       int       `json:"tokens_out"`
	TokensCached    int       `json:"tokens_cached"`
	TokensNewCache  int       `json:"tokens_new_cache"`
	TokensTotal     int       `json:"tokens_total"`
	DurationMs      int64     `json:"duration_ms"`
}

// Recorder appends usage entries to JSONL files on disk.
type Recorder struct {
	baseDir string
	mu      sync.Mutex
	writers map[string]*bufferedFile
	done    chan struct{}
}

type bufferedFile struct {
	f *os.File
	w *bufio.Writer
}

// NewRecorder creates a Recorder that writes to baseDir.
func NewRecorder(baseDir string) *Recorder {
	r := &Recorder{
		baseDir: baseDir,
		writers: make(map[string]*bufferedFile),
		done:    make(chan struct{}),
	}
	go r.flushLoop()
	return r
}

func (r *Recorder) flushLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.flushAll()
		case <-r.done:
			return
		}
	}
}

func (r *Recorder) flushAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, bf := range r.writers {
		bf.w.Flush()
	}
}

// Close flushes and closes all open files.
func (r *Recorder) Close() {
	close(r.done)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, bf := range r.writers {
		bf.w.Flush()
		bf.f.Close()
		delete(r.writers, k)
	}
}

// sanitizeModel replaces / with __ for filesystem safety.
func sanitizeModel(model string) string {
	return strings.ReplaceAll(model, "/", "__")
}

func (r *Recorder) filePath(entry Entry) string {
	year := entry.Timestamp.Format("2006")
	date := entry.Timestamp.Format("2006-01-02")
	model := sanitizeModel(entry.Model)
	if model == "" {
		model = "_unknown"
	}
	return filepath.Join(r.baseDir, entry.AccountID, year, fmt.Sprintf("%s_%s.jsonl", date, model))
}

// Record appends an entry to the appropriate JSONL file.
func (r *Recorder) Record(entry Entry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	entry.ReasoningEffort = ClassifyReasoningEffort(entry.ReasoningEffort, nil)
	path := r.filePath(entry)

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	bf, ok := r.writers[path]
	if !ok {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		bf = &bufferedFile{f: f, w: bufio.NewWriter(f)}
		r.writers[path] = bf
	}

	bf.w.Write(data)
	bf.w.WriteByte('\n')
}

// BaseDir returns the base directory for stats files.
func (r *Recorder) BaseDir() string {
	return r.baseDir
}
