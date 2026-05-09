// Command sniper is the entry point for the WhatsApp sniper bot.
//
// Responsibilities:
//   - Load configuration from environment
//   - Configure structured JSON logging (slog)
//   - Bring up the Postgres-backed whatsmeow client
//   - Start the HTTP server (health, pair, status)
//   - Handle SIGINT/SIGTERM for clean shutdown (flushes whatsmeow state)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wbot/internal/config"
	"wbot/internal/httpd"
	"wbot/internal/sniper"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Use a plain logger here since slog isn't set up yet
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// Structured JSON logs — Render captures stdout and Neon-like services parse JSON well
	setupLogger(cfg.LogLevel)
	slog.Info("boot", "config", cfg.Summary())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ----------------------------------------------------------------
	// Build the sniper (connects to Postgres, runs migrations)
	// ----------------------------------------------------------------
	s, err := sniper.New(ctx, cfg)
	if err != nil {
		slog.Error("sniper init failed", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	// ----------------------------------------------------------------
	// HTTP server: bind first so Render's health check passes during
	// the WhatsApp Noise/WebSocket handshake.
	// ----------------------------------------------------------------
	httpHandler := httpd.New(cfg, s)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		slog.Info("http listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "err", err)
			cancel()
		}
	}()

	// ----------------------------------------------------------------
	// Start whatsmeow client (connects, enters pairing or hot mode)
	// ----------------------------------------------------------------
	if err := s.Start(); err != nil {
		slog.Error("sniper start failed", "err", err)
		os.Exit(1)
	}

	// ----------------------------------------------------------------
	// Graceful shutdown on SIGINT / SIGTERM
	// ----------------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())
	case <-ctx.Done():
		slog.Info("context cancelled — shutting down")
	}

	// Give whatsmeow a bounded window to flush any in-flight crypto state
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	slog.Info("shutdown complete")
}

func setupLogger(level string) {
	lvl := slog.LevelInfo
	switch strings.ToUpper(level) {
	case "DEBUG":
		lvl = slog.LevelDebug
	case "WARN":
		lvl = slog.LevelWarn
	case "ERROR":
		lvl = slog.LevelError
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Shorten time key for log density
			if a.Key == slog.TimeKey {
				a.Key = "t"
			}
			return a
		},
	}
	h := slog.NewJSONHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(h))
}
