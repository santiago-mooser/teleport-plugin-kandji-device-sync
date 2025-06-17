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
	config         *config.Config
	log            *slog.Logger
}

// New creates a new Syncer.
func New(kClient *kandji.Client, tClient *teleport.Client, cfg *config.Config, log *slog.Logger) *Syncer {
	return &Syncer{
		kandjiClient:   kClient,
		teleportClient: tClient,
		config:         cfg,
		log:            log,
	}
}

// Run starts the synchronization loop, running at the specified interval.
func (s *Syncer) Run(ctx context.Context, syncInterval time.Duration) {
	s.log.Info("Starting sync process",
		"interval", syncInterval.String(),
		"on_missing", s.config.OnMissing,
		"sync_devices_without_owners", s.config.SyncDevicesWithoutOwners,
		"include_tags", s.config.IncludeTags,
		"exclude_tags", s.config.ExcludeTags)

	// Connect to Teleport
	if err := s.teleportClient.Connect(ctx, s.config.Teleport); err != nil {
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
	s.log.Debug("Successfully fetched devices from Teleport", "count", len(teleportAssetTags))

	// Get devices from Kandji
	kandjiDevices, err := s.kandjiClient.GetDevices(ctx)
	if err != nil {
		s.log.Error("Failed to get devices from Kandji", "error", err)
		return
	}
	s.log.Debug("Successfully fetched devices from Kandji", "count", len(kandjiDevices))

	// Create maps for efficient lookup
	teleportDeviceMap := make(map[string]struct{}, len(teleportAssetTags))
	for _, tag := range teleportAssetTags {
		teleportDeviceMap[tag] = struct{}{}
	}

	kandjiDeviceMap := make(map[string]struct{}, len(kandjiDevices))
	for _, device := range kandjiDevices {
		if device.SerialNumber != "" {
			kandjiDeviceMap[device.SerialNumber] = struct{}{}
		}
	}

	// Filter devices where the platform is "iPhone" or "iPad"
	var filteredKandjiDevices []kandji.Device
	for _, device := range kandjiDevices {
		if device.Platform != "iPhone" && device.Platform != "iPad" {
			filteredKandjiDevices = append(filteredKandjiDevices, device)
		}
	}
	s.log.Debug("Filtered devices with wrong platform: ", "count", len(kandjiDevices)-len(filteredKandjiDevices))
	kandjiDevices = filteredKandjiDevices

	// Filter devices based on sync_devices_without_owners setting
	var devicesToSync []kandji.Device
	var skippedWithoutOwners int

	for _, device := range kandjiDevices {
		if device.SerialNumber == "" {
			s.log.Warn("Skipping device with empty serial number", "device_name", device.DeviceName)
			continue
		}

		// Check if device has owner when sync_devices_without_owners is false
		if !s.config.SyncDevicesWithoutOwners && device.UserEmail == "" {
			skippedWithoutOwners++
			continue
		}

		devicesToSync = append(devicesToSync, device)
	}

	if skippedWithoutOwners > 0 {
		s.log.Debug("Skipped devices without owners", "count", skippedWithoutOwners)
	}

	// if includeTags is not empty, only include devices with the tags in the IncludeTags list
	if len(s.config.IncludeTags) > 0 {
		var filteredDevices []kandji.Device
		for _, device := range devicesToSync {
			if s.deviceHasAnyTag(device, s.config.IncludeTags) {
				filteredDevices = append(filteredDevices, device)
			}
		}
		s.log.Debug("Included devices after applying includeTags", "count", len(filteredDevices))
		devicesToSync = filteredDevices
	}

	// If excludeTags is not empty, filter any devices that include tags in the excludeTags list
	if len(s.config.ExcludeTags) > 0 {
		var filteredDevices []kandji.Device
		for _, device := range devicesToSync {
			if !s.deviceHasAnyTag(device, s.config.ExcludeTags) {
				filteredDevices = append(filteredDevices, device)
			}
		}
		s.log.Debug("Excluded devices after applying excludeTags", "count", len(filteredDevices)-len(devicesToSync))
		devicesToSync = filteredDevices
	}

	s.log.Debug("Total devices in Kandji that pass filters", "count", len(devicesToSync))

	// Find new devices that need to be added to Teleport
	var newDevices []kandji.Device
	for _, device := range devicesToSync {
		if _, exists := teleportDeviceMap[device.SerialNumber]; !exists {
			s.log.Debug("New device found",
				"serial_number", device.SerialNumber,
				"device_name", device.DeviceName,
				"platform", device.Platform,
				"owner", device.UserEmail)
			newDevices = append(newDevices, device)
		}
	}
	// Log new devices found
	if len(newDevices) > 0 {
		s.log.Debug("New devices found", "count", len(newDevices))
	}

	// Create a map for efficient lookup of devices to sync
	devicesToSyncMap := make(map[string]struct{}, len(devicesToSync))
	for _, device := range devicesToSync {
		if device.SerialNumber != "" {
			devicesToSyncMap[device.SerialNumber] = struct{}{}
		}
	}

	// Remove all devices from KandjiDeviceMap that don't pass the filters (devicesToSync)
	for serial := range kandjiDeviceMap {
		if _, exists := devicesToSyncMap[serial]; !exists {
			delete(kandjiDeviceMap, serial)
		}
	}

	// Handle devices that are missing from Kandji but exist in Teleport
	var missingDevices []string
	if s.config.OnMissing != "ignore" {
		for _, teleportSerial := range teleportAssetTags {
			if _, exists := kandjiDeviceMap[teleportSerial]; !exists {
				missingDevices = append(missingDevices, teleportSerial)
			}
		}

		if len(missingDevices) > 0 {
			s.handleMissingDevices(ctx, missingDevices)
		}
	}

	s.log.Debug("Total devices to sync", "count", len(newDevices))

	// Process new devices in bulk if any found
	if len(newDevices) > 0 {
		s.log.Debug("Processing new devices in bulk", "count", len(newDevices), "batch_size", s.config.Batch.Size)

		result, err := s.teleportClient.AddDevices(ctx, newDevices, s.config.Batch.Size)
		if err != nil {
			s.log.Error("Failed to process device batch", "error", err)
			return
		}

		// Log batch results
		s.log.Debug("Bulk device creation completed",
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
			"eligible_devices", len(devicesToSync),
			"new_devices_found", len(newDevices),
			"successfully_added", result.SuccessCount,
			"deleted_devices", len(missingDevices))
	} else {
		s.log.Info("Sync cycle complete - no new devices found",
			"kandji_devices_total", len(kandjiDevices),
			"eligible_devices", len(devicesToSync),
			"deleted_devices", len(missingDevices))
	}
}

// deviceHasAnyTag checks if a device has any of the specified tags
func (s *Syncer) deviceHasAnyTag(device kandji.Device, includeTags []string) bool {
	for _, deviceTag := range device.Tags {
		for _, includeTag := range includeTags {
			if deviceTag == includeTag {
				return true
			}
		}
	}
	return false
}

// handleMissingDevices processes devices that are in Teleport but missing from Kandji
func (s *Syncer) handleMissingDevices(ctx context.Context, missingDevices []string) {
	switch s.config.OnMissing {
	case "alert":
		s.log.Warn("Devices found in Teleport but missing from Kandji",
			"count", len(missingDevices),
			"action", "alert_only")
		for _, serial := range missingDevices {
			s.log.Warn("Missing device", "serial_number", serial)
		}

	case "delete":
		s.log.Info("Deleting devices in Teleport that are not present in Kandji",
			"count", len(missingDevices),
			"batch_size", s.config.Batch.Size)

		result, err := s.teleportClient.DeleteDevices(ctx, missingDevices, s.config.Batch.Size)
		if err != nil {
			s.log.Error("Failed to delete missing devices", "error", err)
			return
		}

		// Log deletion results
		s.log.Info("Bulk device deletion completed",
			"success_count", result.SuccessCount,
			"failed_count", len(result.FailedDevices),
			"error_count", len(result.Errors))

		// Log individual failures for debugging
		for _, failedDevice := range result.FailedDevices {
			s.log.Error("Failed to delete device",
				"serial_number", failedDevice.SerialNumber,
				"error", failedDevice.Error)
		}

		// Log general errors
		for _, generalError := range result.Errors {
			s.log.Error("Bulk deletion error", "error", generalError)
		}

	default:
		// This should never happen due to config validation, but log just in case
		s.log.Error("Unknown on_missing action", "action", s.config.OnMissing)
	}
}
