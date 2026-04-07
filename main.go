package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/breakawaydata/orchard-gh-bridge/config"
	"github.com/breakawaydata/orchard-gh-bridge/health"
	"github.com/breakawaydata/orchard-gh-bridge/manager"
	"github.com/breakawaydata/orchard-gh-bridge/orchard"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	if *configPath == "" {
		*configPath = os.Getenv("CONFIG_PATH")
	}
	if *configPath == "" {
		*configPath = "/config/config.yaml"
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := setupLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	orchardClient := orchard.NewClient(cfg.Orchard, logger)

	healthSrv := health.NewServer(cfg.Health.Port, orchardClient, logger)
	go healthSrv.Start()

	mgr, err := manager.New(cfg, orchardClient, logger)
	if err != nil {
		logger.Error("failed to create manager", "error", err)
		os.Exit(1)
	}

	logger.Info("starting orchard-gh-bridge",
		"maxVMs", cfg.MaxVMs,
		"scaleSets", len(cfg.ScaleSets),
	)

	if err := mgr.Run(ctx); err != nil {
		logger.Error("manager exited with error", "error", err)
		os.Exit(1)
	}
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
