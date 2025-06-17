package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"teleport-plugin-kandji-device-syncer/config"
	"teleport-plugin-kandji-device-syncer/internal/ratelimit"
	"teleport-plugin-kandji-device-syncer/kandji"
	"teleport-plugin-kandji-device-syncer/syncer"
	"teleport-plugin-kandji-device-syncer/teleport"
)

func main() {
	// Load configuration first to get log level
	cfg, err := config.LoadConfig()
	if err != nil {
		// Use default logger for this error since we don't have config yet
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Setup structured logging with configured level
	var logLevel slog.Level
	err = logLevel.UnmarshalText([]byte(cfg.Log.Level))
	if err != nil {
		slog.Error("Invalid log level", "level", cfg.Log.Level, "error", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	// Create rate limiter
	rateLimiter := ratelimit.New(ratelimit.Config{
		KandjiRequestsPerSecond:   cfg.RateLimits.KandjiRequestsPerSecond,
		TeleportRequestsPerSecond: cfg.RateLimits.TeleportRequestsPerSecond,
		BurstCapacity:             cfg.RateLimits.BurstCapacity,
	})

	// Create clients for Kandji and Teleport
	kandjiClient, err := kandji.NewClient(cfg.Kandji, rateLimiter)
	if err != nil {
		log.Error("Failed to create Kandji client", "error", err)
		os.Exit(1)
	}
	teleportClient := teleport.NewClient(cfg.Teleport, rateLimiter)

	// Create and start the syncer
	syncService := syncer.New(kandjiClient, teleportClient, cfg, log)

	// Set up context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Info("Shutdown signal received, stopping service...")
		cancel()
	}()

	// Start the main sync loop
	syncService.Run(ctx, cfg.SyncInterval)

	log.Info("Service has shut down gracefully.")
}
