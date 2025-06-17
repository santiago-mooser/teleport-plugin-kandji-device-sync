package syncer

import (
	"context"
	"log/slog"
	"time"

	"teleport-plugin-kandji-device-syncer/config"
	"teleport-plugin-kandji-device-syncer/kandji"
	"teleport-plugin-kandji-device-syncer/teleport"
)

// Syncer orchestrates the synchronization from Kandji to Teleport.
type Syncer struct {
	kandjiClient   *kandji.Client
	teleportClient *teleport.Client
	teleportConfig config.TeleportConfig
	batchConfig    config.BatchConfig
	log            *slog.Logger
}

// New creates a new Syncer.
func New(kClient *kandji.Client, tClient *teleport.Client, tConfig config.TeleportConfig, bConfig config.BatchConfig, log *slog.Logger) *Syncer {
	return &Syncer{
		kandjiClient:   kClient,
		teleportClient: tClient,
		teleportConfig: tConfig,
		batchConfig:    bConfig,
		log:            log,
	}
}

// Run starts the synchronization loop, running at the specified interval.
func (s *Syncer) Run(ctx context.Context, syncInterval time.Duration) {
	s.log.Info("Starting sync process", "interval", syncInterval.String())

	// Connect to Teleport
	if err := s.teleportClient.Connect(ctx, s.teleportConfig); err != nil {
		s.log.Error("Failed to connect to Teleport", "error", err)
		return
	}
	defer s.teleportClient.Close()

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	// Run a sync immediately on start-up
	s.Sync(ctx)

	for {
		select {
		case <-ticker.C:
			s.Sync(ctx)
		case <-ctx.Done():
			s.log.Info("Sync process stopping due to context cancellation.")
			return
		}
	}
}

// Sync performs a single synchronization cycle.
func (s *Syncer) Sync(ctx context.Context) {
	s.log.Info("Starting new sync cycle")

	// Get trusted devices from Teleport
	teleportAssetTags, err := s.teleportClient.GetTrustedDevices(ctx)
	if err != nil {
		s.log.Error("Failed to get trusted devices from Teleport", "error", err)
		return
	}
	s.log.Info("Successfully fetched devices from Teleport", "count", len(teleportAssetTags))

	// Get devices from Kandji
	kandjiDevices, err := s.kandjiClient.GetDevices(ctx)
	if err != nil {
		s.log.Error("Failed to get devices from Kandji", "error", err)
		return
	}
	s.log.Info("Successfully fetched devices from Kandji", "count", len(kandjiDevices))

	// Create a map for efficient lookup of Teleport devices
	teleportDeviceMap := make(map[string]struct{}, len(teleportAssetTags))
	for _, tag := range teleportAssetTags {
		teleportDeviceMap[tag] = struct{}{}
	}

	// Collect new devices for bulk processing
	var newDevices []kandji.Device
	for _, device := range kandjiDevices {
		if device.SerialNumber == "" {
			s.log.Warn("Skipping device with empty serial number", "device_name", device.DeviceName)
			continue
		}

		if _, exists := teleportDeviceMap[device.SerialNumber]; !exists {
			s.log.Debug("New device found", "serial_number", device.SerialNumber, "device_name", device.DeviceName, "platform", device.Platform)
			newDevices = append(newDevices, device)
		}
	}

	// Process new devices in bulk if any found
	if len(newDevices) > 0 {
		s.log.Info("Processing new devices in bulk", "count", len(newDevices), "batch_size", s.batchConfig.Size)

		result, err := s.teleportClient.AddDevices(ctx, newDevices, s.batchConfig.Size)
		if err != nil {
			s.log.Error("Failed to process device batch", "error", err)
			return
		}

		// Log batch results
		s.log.Info("Bulk device creation completed",
			"success_count", result.SuccessCount,
			"failed_count", len(result.FailedDevices),
			"error_count", len(result.Errors))

		// Log individual failures for debugging
		for _, failedDevice := range result.FailedDevices {
			s.log.Error("Failed to add device",
				"serial_number", failedDevice.SerialNumber,
				"error", failedDevice.Error)
		}

		// Log general errors
		for _, generalError := range result.Errors {
			s.log.Error("Bulk operation error", "error", generalError)
		}

		s.log.Info("Sync cycle complete",
			"kandji_devices_total", len(kandjiDevices),
			"new_devices_found", len(newDevices),
			"successfully_added", result.SuccessCount)
	} else {
		s.log.Info("Sync cycle complete - no new devices found",
			"kandji_devices_total", len(kandjiDevices))
	}
}
