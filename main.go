package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/whtsky/copilot2api/anthropic"
	"github.com/whtsky/copilot2api/auth"
	"github.com/whtsky/copilot2api/control"
	debugpkg "github.com/whtsky/copilot2api/debug"
	"github.com/whtsky/copilot2api/gateway"
	"github.com/whtsky/copilot2api/gemini"
	"github.com/whtsky/copilot2api/internal/models"
	"github.com/whtsky/copilot2api/internal/upstream"
	"github.com/whtsky/copilot2api/proxy"
	"github.com/whtsky/copilot2api/stats"
	"github.com/whtsky/copilot2api/storage"
)

var version = "dev"
var commit = "dev"

func buildCommit() string {
	if commit != "" && commit != "dev" {
		return shortCommit(commit)
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return shortCommit(setting.Value)
			}
		}
	}

	return "dev"
}

func shortCommit(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func main() {
	var (
		port        = flag.Int("port", 0, "Server port (env: COPILOT2API_PORT, default: 7777)")
		controlPort = flag.Int("control-port", 0, "Control plane port (env: COPILOT2API_CONTROL_PORT, default: 7778)")
		host        = flag.String("host", "", "Server host (env: COPILOT2API_HOST, default: 127.0.0.1)")
		tokenDir    = flag.String("token-dir", "", "Token storage directory (env: COPILOT2API_TOKEN_DIR, default: ~/.config/copilot2api)")
		showVersion = flag.Bool("version", false, "Show version and exit")
		debug       = flag.Bool("debug", false, "Enable debug logging (env: COPILOT2API_DEBUG)")
	)
	flag.Parse()

	// Apply debug env var
	if !*debug {
		if v := os.Getenv("COPILOT2API_DEBUG"); v != "" {
			if enabled, err := strconv.ParseBool(v); err == nil {
				*debug = enabled
			}
		}
	}

	// Apply env var defaults
	if *host == "" {
		if v := os.Getenv("COPILOT2API_HOST"); v != "" {
			*host = v
		} else {
			*host = "127.0.0.1"
		}
	}
	if *port == 0 {
		if v := os.Getenv("COPILOT2API_PORT"); v != "" {
			if p, err := strconv.Atoi(v); err == nil {
				*port = p
			}
		}
		if *port == 0 {
			*port = 7777
		}
	}
	if *controlPort == 0 {
		if v := os.Getenv("COPILOT2API_CONTROL_PORT"); v != "" {
			if p, err := strconv.Atoi(v); err == nil {
				*controlPort = p
			}
		}
		if *controlPort == 0 {
			*controlPort = 7778
		}
	}

	if *showVersion {
		fmt.Printf("copilot2api version %s\n", version)
		os.Exit(0)
	}

	// Set up logging
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// Determine token directory
	if *tokenDir == "" {
		if v := os.Getenv("COPILOT2API_TOKEN_DIR"); v != "" {
			*tokenDir = v
		} else {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				slog.Error("failed to get home directory", "error", err)
				os.Exit(1)
			}
			*tokenDir = filepath.Join(homeDir, ".config", "copilot2api")
		}
	}

	// Initialize storage backend
	storageMode := os.Getenv("STORAGE")
	if storageMode == "" {
		storageMode = "file"
	}

	var backend storage.Backend
	switch storageMode {
	case "file":
		fb, err := storage.NewFileBackend(*tokenDir)
		if err != nil {
			slog.Error("failed to initialize file backend", "error", err)
			os.Exit(1)
		}
		backend = fb
	case "db":
		masterKey, err := storage.GetMasterKey()
		if err != nil {
			slog.Error("failed to get master key", "error", err)
			os.Exit(1)
		}
		encryptor := storage.NewEncryptor(masterKey)
		db, err := storage.NewDBBackend(encryptor)
		if err != nil {
			slog.Error("failed to initialize database backend", "error", err)
			os.Exit(1)
		}

		// One-time file→DB migration if IMPORT_FILE_TO_DB is set
		if importDir := os.Getenv("IMPORT_FILE_TO_DB"); importDir != "" {
			fb, err := storage.NewFileBackend(importDir)
			if err != nil {
				slog.Error("failed to initialize file backend for import", "dir", importDir, "error", err)
				os.Exit(1)
			}
			migrated, err := storage.MigrateFileToDB(context.Background(), fb, db)
			if err != nil {
				slog.Error("file→DB migration failed", "error", err)
				os.Exit(1)
			}
			slog.Info("file→DB migration complete", "migrated", migrated)
		}

		backend = db
	default:
		slog.Error("unsupported STORAGE mode", "mode", storageMode)
		os.Exit(1)
	}

	slog.Info("storage backend initialized", "mode", storageMode)

	// Initialize account manager
	accountManager, err := auth.NewAccountManager(backend)
	if err != nil {
		slog.Error("failed to initialize account manager", "error", err)
		os.Exit(1)
	}
	slog.Info("initialized accounts", "count", accountManager.Count())

	// Authenticate all existing accounts at startup
	ctx := context.Background()
	if accountManager.Count() > 0 {
		if err := accountManager.EnsureAllAuthenticated(ctx); err != nil {
			slog.Error("authentication failed", "error", err)
			os.Exit(1)
		}
	}

	// Shared HTTP transport
	transport := upstream.NewTransport()

	// Models cache — pulls a current account's upstream client on every fetch
	// so accounts added via the control plane after startup also work.
	modelsCache := models.NewCache(func() *upstream.Client {
		accs := accountManager.ListAccounts()
		if len(accs) == 0 {
			return nil
		}
		client, ok := accountManager.GetClient(accs[0].ID)
		if !ok {
			return nil
		}
		tp := auth.NewAccountTokenProvider(client)
		return upstream.NewClient(tp, transport)
	}, 5*time.Minute)

	// Stats recorder (opt-in via COPILOT2API_STATS_ENABLED)
	statsEnabled := false
	if v := os.Getenv("COPILOT2API_STATS_ENABLED"); v == "true" || v == "1" {
		statsEnabled = true
	}
	statsDir := os.Getenv("COPILOT2API_STATS_DIR")
	if statsDir == "" {
		homeDir, _ := os.UserHomeDir()
		statsDir = filepath.Join(homeDir, ".config", "copilot2api", "stats")
	}
	var recorder *stats.Recorder
	var pricingCache *stats.PricingCache
	if statsEnabled {
		recorder = stats.NewRecorder(statsDir)
		defer recorder.Close()
		pricingCache = stats.NewPricingCache(statsDir)
		defer pricingCache.Close()
		slog.Info("stats enabled", "dir", statsDir)
	} else {
		slog.Info("stats disabled (set COPILOT2API_STATS_ENABLED=true to enable)")
	}

	// Initialize debug capture (opt-in via COPILOT2API_DEBUG_MODELS)
	debugpkg.Init(filepath.Dir(statsDir))

	// Set up proxy mux with path-based routing
	mux := http.NewServeMux()

	// All API routes require account_id: /api/{account_id}/v1/...
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		handleAccountRoute(w, r, accountManager, transport, modelsCache, recorder)
	})

	// Gateway load-balancing route: /gw/api/...
	gwHandler := gateway.NewHandler(accountManager, transport, modelsCache)
	gwHandler.Recorder = recorder
	mux.Handle("/gw/api/", gwHandler)



	// Usage endpoint (aggregates all accounts)
	mux.HandleFunc("/usage", func(w http.ResponseWriter, r *http.Request) {
		// Pick first available account for usage
		accounts := accountManager.ListAccounts()
		if len(accounts) == 0 {
			proxy.WriteOpenAIError(w, http.StatusServiceUnavailable, proxy.OpenAIErrorTypeServerError, "no accounts configured")
			return
		}
		client, _ := accountManager.GetClient(accounts[0].ID)
		tp := auth.NewAccountTokenProvider(client)
		proxyHandler := proxy.NewHandler(tp, transport, modelsCache, &aggregateUsageProvider{am: accountManager})
		proxyHandler.HandleUsage(w, r)
	})


	// Create proxy server with optional API_TOKEN auth
	var proxyHandler http.Handler = logAllRequests(mux)
	if apiToken := os.Getenv("API_TOKEN"); apiToken != "" {
		proxyHandler = apiTokenAuth(apiToken, proxyHandler)
	}
	proxyServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *port),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		Handler:           proxyHandler,
	}

	// Create control plane server
	adminToken := os.Getenv("ADMIN_TOKEN")
	controlServer := control.NewServer(accountManager, adminToken, statsDir, pricingCache, modelsCache, buildCommit())
	controlHTTP := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *controlPort),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		Handler:           controlServer.Handler(),
	}

	// Start servers
	serverErr := make(chan error, 2)
	go func() {
		slog.Info("starting proxy server", "host", *host, "port", *port)
		if err := proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()
	go func() {
		slog.Info("starting control plane server", "host", *host, "port", *controlPort)
		if err := controlHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Wait for interrupt or error
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case err := <-serverErr:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}

	slog.Info("shutting down servers")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proxyServer.Shutdown(shutdownCtx)
	controlHTTP.Shutdown(shutdownCtx)
	slog.Info("servers stopped")
}

// handleAccountRoute routes /api/{account_id}/... — account_id is always required.
// The remainder after /api/{account_id}/ is forwarded as-is (e.g. /v1/chat/completions, /v1/messages, /v1beta/models/...).
func handleAccountRoute(w http.ResponseWriter, r *http.Request, am *auth.AccountManager, transport *http.Transport, mc *models.Cache, recorder *stats.Recorder) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")

	// Parse as /api/{account_id}/...
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		proxy.WriteOpenAIError(w, http.StatusBadRequest, proxy.OpenAIErrorTypeInvalidRequest, "account_id required in path: /api/{account_id}/v1/...")
		return
	}

	accountID := parts[0]
	remainder := "/" + parts[1] // e.g. /v1/chat/completions

	client, ok := am.GetClient(accountID)
	if !ok {
		proxy.WriteOpenAIError(w, http.StatusNotFound, proxy.OpenAIErrorTypeInvalidRequest, fmt.Sprintf("account %q not found", accountID))
		return
	}

	// Rewrite path to the remainder
	r.URL.Path = remainder
	tp := auth.NewAccountTokenProvider(client)
	tp.AccountID = accountID
	handleWithTokenProvider(w, r, tp, transport, mc, recorder)
}

// handleWithTokenProvider dispatches a request using a specific token provider.
func handleWithTokenProvider(w http.ResponseWriter, r *http.Request, tp upstream.TokenProvider, transport *http.Transport, mc *models.Cache, recorder *stats.Recorder) {
	path := r.URL.Path

	switch {
	case path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages"):
		handler := anthropic.NewHandler(tp, transport, mc)
		handler.StatsRecorder = recorder
		handler.ServeHTTP(w, r)
	case strings.HasPrefix(path, "/v1beta/models"):
		handler := gemini.NewHandler(tp, transport, mc)
		handler.ServeHTTP(w, r)
	default:
		handler := proxy.NewHandler(tp, transport, mc, nil)
		handler.StatsRecorder = recorder
		handler.ServeHTTP(w, r)
	}
}

// aggregateUsageProvider implements proxy.UsageProvider for all accounts.
type aggregateUsageProvider struct {
	am *auth.AccountManager
}

func (a *aggregateUsageProvider) GetUsageInfo(ctx context.Context) (interface{}, error) {
	accounts := a.am.ListAccounts()
	var results []interface{}
	for _, acc := range accounts {
		client, ok := a.am.GetClient(acc.ID)
		if !ok {
			continue
		}
		info, err := client.GetUsageInfo(ctx)
		if err != nil {
			continue
		}
		results = append(results, info)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no usage info available")
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return results, nil
}

func apiTokenAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		xApiKey := r.Header.Get("x-api-key")
		if auth != "Bearer "+token && xApiKey != token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"invalid or missing API_TOKEN","type":"authentication_error","code":"unauthorized"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logAllRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("incoming request", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery)
		next.ServeHTTP(w, r)
	})
}
