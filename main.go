package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"teleport-plugin-kandji-device-sync/config"
	"teleport-plugin-kandji-device-sync/internal/ratelimit"
	"teleport-plugin-kandji-device-sync/kandji"
	"teleport-plugin-kandji-device-sync/syncer"
	"teleport-plugin-kandji-device-sync/teleport"
)

var (
	Version = "dev"

	Commit = "n/a"

	CommitDate = "n/a"

	TreeState = "n/a"
)

func printVersion() {
	showVersion := flag.Bool("version", false, "show version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("%s, %s, %s, %s\n", Version, Commit, CommitDate, TreeState)
		os.Exit(0)
	}
}

func main() {
	printVersion()
	// Load configuration first to get log level
	cfg, err := config.LoadConfig()
	if err != nil {
		// Use default logger for this error since we don't have config yet
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// debug log
	slog.Debug("Configuration loaded successfully", "config", cfg)

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
	syncService := syncer.New(kandjiClient, teleportClient, cfg, rateLimiter, log)

	// Set up context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a wait group for goroutines
	var wg sync.WaitGroup

	// Start identity refresh routine if configured
	if cfg.Teleport.IdentityRefreshInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runIdentityRefresh(ctx, teleportClient, cfg.Teleport, log)
		}()
		log.Info("Identity refresh enabled", "interval", cfg.Teleport.IdentityRefreshInterval)
	}

	// Listen for interrupt signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the main sync loop in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		syncService.Run(ctx, cfg.SyncInterval)
	}()

	log.Info("Service started successfully")

	// Wait for shutdown signal
	<-sigChan
	log.Info("Shutdown signal received, stopping service...")
	cancel()

	// Create shutdown timeout only after receiving signal
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Wait for all goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info("All services stopped gracefully")
	case <-shutdownCtx.Done():
		log.Warn("Shutdown timeout reached, forcing exit")
	}

	log.Info("Service has shut down gracefully.")
}

// runIdentityRefresh runs the identity refresh routine
func runIdentityRefresh(ctx context.Context, client *teleport.Client, cfg config.TeleportConfig, log *slog.Logger) {
	ticker := time.NewTicker(cfg.IdentityRefreshInterval)
	defer ticker.Stop()

	log.Info("Starting identity refresh routine", "interval", cfg.IdentityRefreshInterval)

	for {
		select {
		case <-ctx.Done():
			log.Info("Identity refresh routine stopped")
			return
		case <-ticker.C:
			log.Debug("Attempting to refresh identity")
			if err := client.RefreshIdentity(ctx, cfg); err != nil {
				log.Error("Failed to refresh identity", "error", err)
			} else {
				log.Info("Identity refreshed successfully")
			}
		}
	}
}
