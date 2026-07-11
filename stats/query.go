package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AggregatedEntry is a daily summary returned by the query API.
type AggregatedEntry struct {
	Date            string `json:"date"`
	AccountID       string `json:"account_id"`
	Username        string `json:"username"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	TokensIn        int    `json:"tokens_in"`
	TokensOut       int    `json:"tokens_out"`
	TokensCached    int    `json:"tokens_cached"`
	TokensNewCache  int    `json:"tokens_new_cache"`
	TokensTotal     int    `json:"tokens_total"`
	RequestCount    int    `json:"request_count"`
}

// AccountSummary is returned by the accounts listing endpoint.
type AccountSummary struct {
	AccountID string `json:"account_id"`
	Username  string `json:"username"`
}

// aggKey is the map key for aggregating stats by date/account/model/effort.
type aggKey struct {
	date, accountID, model, reasoningEffort string
}

// Query reads JSONL files and returns aggregated daily stats.
func Query(baseDir, accountID, model string, start, end time.Time) ([]AggregatedEntry, error) {
	return QueryWithReasoningEffort(baseDir, accountID, model, "", start, end)
}

// QueryWithReasoningEffort reads JSONL files and optionally filters by effort.
func QueryWithReasoningEffort(baseDir, accountID, model, reasoningEffort string, start, end time.Time) ([]AggregatedEntry, error) {
	agg := make(map[aggKey]*AggregatedEntry)
	lastUsername := make(map[string]string)
	reasoningEffort = strings.ToLower(strings.TrimSpace(reasoningEffort))

	// Determine which account dirs to scan
	var accountDirs []string
	if accountID != "" {
		accountDirs = []string{filepath.Join(baseDir, accountID)}
	} else {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			if os.IsNotExist(err) {
				return []AggregatedEntry{}, nil
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				accountDirs = append(accountDirs, filepath.Join(baseDir, e.Name()))
			}
		}
	}

	for _, accDir := range accountDirs {
		// Walk year subdirs
		years, _ := os.ReadDir(accDir)
		for _, yearEntry := range years {
			if !yearEntry.IsDir() {
				continue
			}
			yearDir := filepath.Join(accDir, yearEntry.Name())
			files, _ := os.ReadDir(yearDir)
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				// Parse date from filename: YYYY-MM-DD_model.jsonl
				parts := strings.SplitN(f.Name(), "_", 2)
				if len(parts) < 2 {
					continue
				}
				fileDate, err := time.Parse("2006-01-02", parts[0])
				if err != nil {
					continue
				}
				if fileDate.Before(start) || fileDate.After(end) {
					continue
				}
				// If model filter set, check filename
				if model != "" {
					fileModel := strings.TrimSuffix(parts[1], ".jsonl")
					if fileModel != sanitizeModel(model) {
						continue
					}
				}

				readJSONLFile(filepath.Join(yearDir, f.Name()), start, end, model, reasoningEffort, agg, lastUsername)
			}
		}
	}

	result := make([]AggregatedEntry, 0, len(agg))
	for _, v := range agg {
		if u, ok := lastUsername[v.AccountID]; ok && v.Username == "" {
			v.Username = u
		}
		result = append(result, *v)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Date != result[j].Date {
			return result[i].Date < result[j].Date
		}
		if result[i].AccountID != result[j].AccountID {
			return result[i].AccountID < result[j].AccountID
		}
		if result[i].Model != result[j].Model {
			return result[i].Model < result[j].Model
		}
		return result[i].ReasoningEffort < result[j].ReasoningEffort
	})
	return result, nil
}

func readJSONLFile(path string, start, end time.Time, modelFilter, reasoningEffortFilter string, agg map[aggKey]*AggregatedEntry, usernames map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		day := e.Timestamp.Format("2006-01-02")
		dayTime, _ := time.Parse("2006-01-02", day)
		if dayTime.Before(start) || dayTime.After(end) {
			continue
		}
		if modelFilter != "" && e.Model != modelFilter {
			continue
		}
		e.ReasoningEffort = ClassifyReasoningEffort(e.ReasoningEffort, nil)
		if reasoningEffortFilter != "" && e.ReasoningEffort != reasoningEffortFilter {
			continue
		}

		k := aggKey{day, e.AccountID, e.Model, e.ReasoningEffort}
		entry, ok := agg[k]
		if !ok {
			entry = &AggregatedEntry{
				Date:            day,
				AccountID:       e.AccountID,
				Username:        e.Username,
				Model:           e.Model,
				ReasoningEffort: e.ReasoningEffort,
			}
			agg[k] = entry
		}
		entry.TokensIn += e.TokensIn
		entry.TokensOut += e.TokensOut
		entry.TokensCached += e.TokensCached
		entry.TokensNewCache += e.TokensNewCache
		entry.TokensTotal += e.TokensTotal
		entry.RequestCount++
		if e.Username != "" {
			usernames[e.AccountID] = e.Username
			entry.Username = e.Username
		}
	}
}

// ListAccounts scans the base directory for account folders.
func ListAccounts(baseDir string) ([]AccountSummary, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []AccountSummary{}, nil
		}
		return nil, err
	}

	var result []AccountSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		accountID := e.Name()
		username := findLatestUsername(filepath.Join(baseDir, accountID))
		result = append(result, AccountSummary{AccountID: accountID, Username: username})
	}
	return result, nil
}

func findLatestUsername(accDir string) string {
	// Walk backwards through year dirs to find most recent file
	years, _ := os.ReadDir(accDir)
	if len(years) == 0 {
		return ""
	}
	// Sort years descending
	sort.Slice(years, func(i, j int) bool {
		return years[i].Name() > years[j].Name()
	})
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(accDir, y.Name()))
		if len(files) == 0 {
			continue
		}
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() > files[j].Name()
		})
		// Read last line of most recent file
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			fp := filepath.Join(accDir, y.Name(), f.Name())
			username := lastUsernameInFile(fp)
			if username != "" {
				return username
			}
		}
	}
	return ""
}

func lastUsernameInFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)
	for scanner.Scan() {
		var e struct {
			Username string `json:"username"`
		}
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Username != "" {
			last = e.Username
		}
	}
	return last
}
