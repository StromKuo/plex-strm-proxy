package main

import (
	"log/slog"
	"os"

	"plex-strm-proxy/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := app.LoadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	server, err := app.NewServer(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize server", "error", err)
		os.Exit(1)
	}

	logger.Info("starting plex-strm-proxy", "listen", cfg.ListenAddr, "plex_upstream_host", cfg.PlexUpstream.Host)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
