package syncer

import (
	"context"
	"log/slog"
	"reflect"
	"time"

	"teleport-plugin-kandji-device-sync/config"
	"teleport-plugin-kandji-device-sync/internal/ratelimit"
	"teleport-plugin-kandji-device-sync/kandji"
	"teleport-plugin-kandji-device-sync/teleport"
)

// Syncer orchestrates the synchronization from Kandji to Teleport.
type Syncer struct {
	kandjiClient   *kandji.Client
	teleportClient *teleport.Client
	config         *config.Config
	rateLimiter    *ratelimit.Limiter
	log            *slog.Logger
}

// New creates a new Syncer.
func New(kClient *kandji.Client, tClient *teleport.Client, cfg *config.Config, rateLimiter *ratelimit.Limiter, log *slog.Logger) *Syncer {
	return &Syncer{
		kandjiClient:   kClient,
		teleportClient: tClient,
		config:         cfg,
		rateLimiter:    rateLimiter,
		log:            log,
	}
}

// Run starts the synchronization loop, running at the specified interval.
func (s *Syncer) Run(ctx context.Context, syncInterval time.Duration) {
	s.log.Info("Starting sync process",
		"interval", syncInterval.String(),
		"on_missing", s.config.OnMissing,
		"sync_devices_without_owners", s.config.Kandji.SyncDevicesWithoutOwners,
		"include_tags", s.config.Kandji.IncludeTags,
		"exclude_tags", s.config.Kandji.ExcludeTags,
		"blueprints_include", s.config.Kandji.BlueprintsInclude,
		"blueprints_exclude", s.config.Kandji.BlueprintsExclude)

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

	// Reload config at the start of each sync cycle to support rotating identity files
	s.reloadConfigIfNeeded(ctx)

	// Get trusted devices from Teleport
	teleportAssetTags, err := s.teleportClient.GetTrustedDevices(ctx)
	if err != nil {
		s.log.Error("Failed to get trusted devices from Teleport", "error", err)
		return
	}
	s.log.Debug("Successfully fetched devices from Teleport", "count", len(teleportAssetTags))

	// Create maps for efficient lookup
	teleportDeviceMap := make(map[string]struct{}, len(teleportAssetTags))
	for _, tag := range teleportAssetTags {
		teleportDeviceMap[tag] = struct{}{}
	}
	// Get devices from Kandji
	kandjiDevices, err := s.kandjiClient.GetDevices(ctx)
	if err != nil {
		s.log.Error("Failed to get devices from Kandji", "error", err)
		return
	}
	s.log.Debug("Successfully fetched devices from Kandji", "count", len(kandjiDevices))

	kandjiDeviceMap := make(map[string]struct{}, len(kandjiDevices))
	for _, device := range kandjiDevices {
		if device.SerialNumber != "" {
			kandjiDeviceMap[device.SerialNumber] = struct{}{}
		}
	}

	// 1. Apply filters
	var filteredKandjiDevices []kandji.Device
	for _, device := range kandjiDevices {
		if device.SerialNumber == "" {
			s.log.Debug("Skipping device with empty serial number", "device_name", device.DeviceName)
			continue
		}
		if !s.config.Kandji.SyncDevicesWithoutOwners && device.UserEmail == "" {
			s.log.Debug("Skipping device without owner", "serial_number", device.SerialNumber)
			continue
		}
		if !s.config.Kandji.SyncMobileDevices && (device.Platform == "iPhone" || device.Platform == "iPad") {
			s.log.Debug("Skipping mobile device", "serial_number", device.SerialNumber)
			continue
		}
		if len(s.config.Kandji.IncludeTags) > 0 && !s.deviceHasAnyTag(device, s.config.Kandji.IncludeTags) {
			s.log.Debug("Skipping device not in include tags", "serial_number", device.SerialNumber)
			continue
		}
		if len(s.config.Kandji.ExcludeTags) > 0 && s.deviceHasAnyTag(device, s.config.Kandji.ExcludeTags) {
			s.log.Debug("Skipping device in exclude tags", "serial_number", device.SerialNumber)
			continue
		}

		// Blueprint filtering
		if !s.deviceMatchesBlueprint(&device) {
			continue
		}

		filteredKandjiDevices = append(filteredKandjiDevices, device)
		s.log.Debug("Including device for sync", "serial_number", device.SerialNumber)
	}
	s.log.Info("Total new devices in Kandji that pass filters", "count", len(filteredKandjiDevices))

	// Find new devices that need to be added to Teleport
	var newDevices []kandji.Device
	for _, device := range filteredKandjiDevices {
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
	devicesToSyncMap := make(map[string]struct{}, len(filteredKandjiDevices))
	for _, device := range filteredKandjiDevices {
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
	deletedDevicesKey := "deleted_devices"
	if s.config.OnMissing != "delete" {
		deletedDevicesKey = "present_devices_missing_from_kandji"
	}
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
			"eligible_devices", len(filteredKandjiDevices),
			"new_devices_found", len(newDevices),
			"successfully_added", result.SuccessCount,
			deletedDevicesKey, len(missingDevices))
	} else {
		s.log.Info("Sync cycle complete - no new devices found",
			"kandji_devices_total", len(kandjiDevices),
			"eligible_devices", len(filteredKandjiDevices),
			deletedDevicesKey, len(missingDevices))
	}
}

// createSet creates a set from a slice of strings for efficient lookups.
func createSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

// deviceMatchesBlueprint checks if a device matches the blueprint filters.
func (s *Syncer) deviceMatchesBlueprint(device *kandji.Device) bool {
	includeIDs := createSet(s.config.Kandji.BlueprintsInclude.BlueprintIDs)
	includeNames := createSet(s.config.Kandji.BlueprintsInclude.BlueprintNames)
	excludeIDs := createSet(s.config.Kandji.BlueprintsExclude.BlueprintIDs)
	excludeNames := createSet(s.config.Kandji.BlueprintsExclude.BlueprintNames)

	// Log device blueprint info for debugging
	s.log.Debug("Checking device blueprint",
		"serial_number", device.SerialNumber,
		"device_blueprint_id", device.BlueprintID,
		"device_blueprint_name", device.BlueprintName,
		"include_ids", includeIDs,
		"blueprint_names", includeNames)

	// Exclude filter has priority
	if _, ok := excludeIDs[device.BlueprintID]; ok {
		s.log.Debug("Device excluded by blueprint ID", "serial_number", device.SerialNumber, "blueprint_id", device.BlueprintID)
		return false
	}
	if _, ok := excludeNames[device.BlueprintName]; ok {
		s.log.Debug("Device excluded by blueprint name", "serial_number", device.SerialNumber, "blueprint_name", device.BlueprintName)
		return false
	}

	// If no include filters are set, all non-excluded devices are included.
	if len(includeIDs) == 0 && len(includeNames) == 0 {
		return true
	}

	// Include filter
	if _, ok := includeIDs[device.BlueprintID]; ok {
		s.log.Debug("Device included by blueprint ID", "serial_number", device.SerialNumber, "blueprint_id", device.BlueprintID)
		return true
	}
	if _, ok := includeNames[device.BlueprintName]; ok {
		s.log.Debug("Device included by blueprint name", "serial_number", device.SerialNumber, "blueprint_name", device.BlueprintName)
		return true
	}

	s.log.Debug("Device did not match any include blueprint filters", "serial_number", device.SerialNumber, "device_blueprint_name", device.BlueprintName, "device_blueprint_id", device.BlueprintID, "include_ids", includeIDs, "blueprint_names", includeNames)
	return false
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

// reloadConfigIfNeeded reloads the configuration and handles any necessary updates
func (s *Syncer) reloadConfigIfNeeded(ctx context.Context) {
	s.log.Debug("Checking for config updates")

	// Load the latest config
	newConfig, err := config.LoadConfig()
	if err != nil {
		s.log.Error("Failed to reload config, continuing with existing config", "error", err)
		return
	}

	// Check if config has changed
	if s.configChanged(newConfig) {
		s.log.Info("Configuration changes detected, updating syncer")
		s.updateConfig(ctx, newConfig)
	} else {
		s.log.Debug("No configuration changes detected")
	}
}

// configChanged compares the current config with a new config to detect changes
func (s *Syncer) configChanged(newConfig *config.Config) bool {
	// Compare critical fields that would require updates
	return s.config.Teleport.IdentityFile != newConfig.Teleport.IdentityFile ||
		s.config.Teleport.ProxyAddr != newConfig.Teleport.ProxyAddr ||
		s.config.Teleport.IdentityRefreshInterval != newConfig.Teleport.IdentityRefreshInterval ||
		!reflect.DeepEqual(s.config.RateLimits, newConfig.RateLimits) ||
		!reflect.DeepEqual(s.config.Kandji, newConfig.Kandji) ||
		s.config.OnMissing != newConfig.OnMissing ||
		!reflect.DeepEqual(s.config.Batch, newConfig.Batch)
}

// updateConfig updates the syncer with new configuration and handles reconnections
func (s *Syncer) updateConfig(ctx context.Context, newConfig *config.Config) {
	oldConfig := s.config

	// Update the config
	s.config = newConfig

	// Check if rate limit settings changed
	if !reflect.DeepEqual(oldConfig.RateLimits, newConfig.RateLimits) {
		s.log.Info("Rate limit settings changed, updating rate limiter",
			"old_kandji_rps", oldConfig.RateLimits.KandjiRequestsPerSecond,
			"new_kandji_rps", newConfig.RateLimits.KandjiRequestsPerSecond,
			"old_teleport_rps", oldConfig.RateLimits.TeleportRequestsPerSecond,
			"new_teleport_rps", newConfig.RateLimits.TeleportRequestsPerSecond,
			"old_burst", oldConfig.RateLimits.BurstCapacity,
			"new_burst", newConfig.RateLimits.BurstCapacity)

		// Create new rate limiter with updated settings
		s.rateLimiter = ratelimit.New(ratelimit.Config{
			KandjiRequestsPerSecond:   newConfig.RateLimits.KandjiRequestsPerSecond,
			TeleportRequestsPerSecond: newConfig.RateLimits.TeleportRequestsPerSecond,
			BurstCapacity:             newConfig.RateLimits.BurstCapacity,
		})

		// Update clients with new rate limiter
		s.updateClientRateLimiters()
	}

	// Check if Teleport connection settings changed
	if oldConfig.Teleport.IdentityFile != newConfig.Teleport.IdentityFile ||
		oldConfig.Teleport.ProxyAddr != newConfig.Teleport.ProxyAddr {
		s.log.Info("Teleport connection settings changed, reconnecting",
			"old_identity_file", oldConfig.Teleport.IdentityFile,
			"new_identity_file", newConfig.Teleport.IdentityFile,
			"old_proxy_addr", oldConfig.Teleport.ProxyAddr,
			"new_proxy_addr", newConfig.Teleport.ProxyAddr)

		s.reconnectToTeleport(ctx, newConfig.Teleport)
	}

	// Log other significant changes
	if !reflect.DeepEqual(oldConfig.Kandji, newConfig.Kandji) {
		s.log.Info("Kandji configuration changed")
	}

	if oldConfig.OnMissing != newConfig.OnMissing {
		s.log.Info("OnMissing behavior changed",
			"old", oldConfig.OnMissing,
			"new", newConfig.OnMissing)
	}

	if !reflect.DeepEqual(oldConfig.Batch, newConfig.Batch) {
		s.log.Info("Batch configuration changed",
			"old_size", oldConfig.Batch.Size,
			"new_size", newConfig.Batch.Size,
			"old_max_concurrent", oldConfig.Batch.MaxConcurrentBatches,
			"new_max_concurrent", newConfig.Batch.MaxConcurrentBatches)
	}
}

// updateClientRateLimiters updates the rate limiters for all clients
func (s *Syncer) updateClientRateLimiters() {
	// Update the teleport client's rate limiter
	s.teleportClient.UpdateRateLimiter(s.rateLimiter)

	// Update the kandji client's rate limiter
	s.kandjiClient.UpdateRateLimiter(s.rateLimiter)
}

// reconnectToTeleport safely reconnects to Teleport with new settings
func (s *Syncer) reconnectToTeleport(ctx context.Context, newTeleportConfig config.TeleportConfig) {
	s.log.Info("Reconnecting to Teleport with updated configuration")

	// Close existing connection
	if err := s.teleportClient.Close(); err != nil {
		s.log.Warn("Error closing existing Teleport connection", "error", err)
	}

	// Establish new connection with updated config
	if err := s.teleportClient.Connect(ctx, newTeleportConfig); err != nil {
		s.log.Error("Failed to reconnect to Teleport with new configuration", "error", err)
		// Note: We continue execution here - the sync will fail but we don't want to crash the whole process
		return
	}

	s.log.Info("Successfully reconnected to Teleport")
}
