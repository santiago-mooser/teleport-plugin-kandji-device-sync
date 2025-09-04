package teleport

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gravitational/teleport/api/client"
	devicetrustv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/devicetrust/v1"

	"teleport-plugin-kandji-device-sync/config"
	"teleport-plugin-kandji-device-sync/internal/ratelimit"
	"teleport-plugin-kandji-device-sync/kandji"
)

// BulkCreateResult holds the results of a bulk device creation operation.
type BulkCreateResult struct {
	SuccessCount  int
	FailedDevices []FailedDevice
	Errors        []error
}

// BulkDeleteResult holds the results of a bulk device deletion operation.
type BulkDeleteResult struct {
	SuccessCount  int
	FailedDevices []FailedDevice
	Errors        []error
}

// FailedDevice represents a device that failed to be created or deleted.
type FailedDevice struct {
	SerialNumber string
	Error        error
}

// HealthStatus tracks the health of the teleport client
type HealthStatus struct {
	TeleportConnected bool
	IdentityValid     bool
	LastRefresh       time.Time
}

// Client wraps the Teleport API client for device trust operations.
type Client struct {
	apiClient    *client.Client
	rateLimiter  *ratelimit.Limiter
	mu           sync.RWMutex
	healthStatus *HealthStatus
}

// NewClient creates a new Teleport API client.
func NewClient(cfg config.TeleportConfig, rateLimiter *ratelimit.Limiter) *Client {
	return &Client{
		rateLimiter: rateLimiter,
		healthStatus: &HealthStatus{
			TeleportConnected: false,
			IdentityValid:     false,
			LastRefresh:       time.Now(),
		},
	}
}

// Connect establishes a connection to the Teleport cluster.
func (c *Client) Connect(ctx context.Context, cfg config.TeleportConfig) error {
	if cfg.ProxyAddr == "" {
		return fmt.Errorf("proxy address is required")
	}
	if cfg.IdentityFile == "" {
		return fmt.Errorf("identity file is required")
	}
	clientConfig := client.Config{
		Addrs: []string{cfg.ProxyAddr},
		Credentials: []client.Credentials{
			client.LoadIdentityFile(cfg.IdentityFile),
		},
	}

	apiClient, err := client.New(ctx, clientConfig)
	if err != nil {
		c.mu.Lock()
		c.healthStatus.TeleportConnected = false
		c.healthStatus.IdentityValid = false
		c.mu.Unlock()
		return fmt.Errorf("failed to create Teleport API client: %w", err)
	}

	c.mu.Lock()
	c.apiClient = apiClient
	c.healthStatus.TeleportConnected = true
	c.healthStatus.IdentityValid = true
	c.healthStatus.LastRefresh = time.Now()
	c.mu.Unlock()
	return nil
}

// Close closes the Teleport API client connection.
func (c *Client) Close() error {
	if c.apiClient != nil {
		return c.apiClient.Close()
	}
	return nil
}

// GetTrustedDevices returns the asset tags (serial numbers) of all registered devices.
func (c *Client) GetTrustedDevices(ctx context.Context) ([]string, error) {
	if c.apiClient == nil {
		return nil, fmt.Errorf("Teleport client not connected")
	}

	// Apply rate limiting
	if c.rateLimiter != nil {
		if err := c.rateLimiter.WaitForTeleport(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter cancelled: %w", err)
		}
	}

	resp, err := c.apiClient.DevicesClient().ListDevices(ctx, &devicetrustv1.ListDevicesRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list trusted devices: %w", err)
	}

	assetTags := make([]string, 0, len(resp.Devices))
	for _, device := range resp.Devices {
		if device.AssetTag != "" {
			assetTags = append(assetTags, device.AssetTag)
		}
	}

	return assetTags, nil
}

// mapPlatformToOSType maps Kandji platform strings to Teleport OSType enum.
func mapPlatformToOSType(platform string) devicetrustv1.OSType {
	switch platform {
	case "Mac", "macOS", "Darwin":
		return devicetrustv1.OSType_OS_TYPE_MACOS
	case "Windows":
		return devicetrustv1.OSType_OS_TYPE_WINDOWS
	case "Linux":
		return devicetrustv1.OSType_OS_TYPE_LINUX
	default:
		return devicetrustv1.OSType_OS_TYPE_UNSPECIFIED
	}
}

// mapKandjiToTeleportDevice converts a Kandji device to a Teleport device.
func mapKandjiToTeleportDevice(kandjiDevice kandji.Device) *devicetrustv1.Device {
	device := &devicetrustv1.Device{
		ApiVersion: "v1",
		OsType:     mapPlatformToOSType(kandjiDevice.Platform),
		AssetTag:   kandjiDevice.SerialNumber,
	}

	// Set owner if available
	if kandjiDevice.UserEmail != "" {
		device.Owner = kandjiDevice.UserEmail
	}

	// Create device profile if we have additional info
	if kandjiDevice.Model != "" || kandjiDevice.OSVersion != "" {
		device.Profile = &devicetrustv1.DeviceProfile{
			// Note: DeviceProfile fields may vary based on Teleport version
			// Add appropriate fields based on your Teleport API version
		}
	}

	return device
}

// AddDevices adds multiple devices to Teleport in bulk operations.
// It processes devices in batches and implements retry logic with exponential backoff.
func (c *Client) AddDevices(ctx context.Context, devices []kandji.Device, batchSize int) (*BulkCreateResult, error) {
	if c.apiClient == nil {
		return nil, fmt.Errorf("Teleport client not connected")
	}

	if len(devices) == 0 {
		return &BulkCreateResult{}, nil
	}

	result := &BulkCreateResult{
		FailedDevices: make([]FailedDevice, 0),
		Errors:        make([]error, 0),
	}

	// Process devices in batches
	for i := 0; i < len(devices); i += batchSize {
		end := i + batchSize
		if end > len(devices) {
			end = len(devices)
		}

		batch := devices[i:end]
		batchResult := c.processBatch(ctx, batch)

		// Aggregate results
		result.SuccessCount += batchResult.SuccessCount
		result.FailedDevices = append(result.FailedDevices, batchResult.FailedDevices...)
		result.Errors = append(result.Errors, batchResult.Errors...)
	}

	return result, nil
}

// processBatch processes a single batch of devices using BulkCreateDevicesRequest.
func (c *Client) processBatch(ctx context.Context, devices []kandji.Device) *BulkCreateResult {
	result := &BulkCreateResult{
		FailedDevices: make([]FailedDevice, 0),
		Errors:        make([]error, 0),
	}

	// Convert Kandji devices to Teleport devices
	teleportDevices := make([]*devicetrustv1.Device, 0, len(devices))
	for _, device := range devices {
		if device.SerialNumber == "" {
			result.FailedDevices = append(result.FailedDevices, FailedDevice{
				SerialNumber: device.DeviceName,
				Error:        fmt.Errorf("empty serial number"),
			})
			continue
		}
		teleportDevices = append(teleportDevices, mapKandjiToTeleportDevice(device))
	}

	if len(teleportDevices) == 0 {
		return result
	}

	// Create bulk request
	req := &devicetrustv1.BulkCreateDevicesRequest{
		Devices:          teleportDevices,
		CreateAsResource: false, // Use standard device creation semantics
	}

	// Implement retry logic with exponential backoff
	maxRetries := 3
	baseDelay := 1 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Apply rate limiting for each attempt
		if c.rateLimiter != nil {
			if err := c.rateLimiter.WaitForTeleport(ctx); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("rate limiter cancelled: %w", err))
				return result
			}
		}

		_, err := c.apiClient.DevicesClient().BulkCreateDevices(ctx, req)
		if err == nil {
			result.SuccessCount = len(teleportDevices)
			return result
		}

		// If this is the last attempt, mark all devices as failed
		if attempt == maxRetries {
			for _, device := range devices {
				if device.SerialNumber != "" {
					result.FailedDevices = append(result.FailedDevices, FailedDevice{
						SerialNumber: device.SerialNumber,
						Error:        err,
					})
				}
			}
			result.Errors = append(result.Errors, fmt.Errorf("failed to create batch after %d attempts: %w", maxRetries+1, err))
			return result
		}

		// Calculate delay with exponential backoff
		delay := baseDelay * time.Duration(1<<attempt)
		select {
		case <-ctx.Done():
			result.Errors = append(result.Errors, ctx.Err())
			return result
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	return result
}

// DeleteDevice deletes a single device from Teleport by serial number.
func (c *Client) DeleteDevice(ctx context.Context, serialNumber string) error {
	if c.apiClient == nil {
		return fmt.Errorf("Teleport client not connected")
	}

	if serialNumber == "" {
		return fmt.Errorf("serial number cannot be empty")
	}

	// Implement retry logic with exponential backoff
	maxRetries := 3
	baseDelay := 1 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Apply rate limiting for each attempt
		if c.rateLimiter != nil {
			if err := c.rateLimiter.WaitForTeleport(ctx); err != nil {
				return fmt.Errorf("rate limiter cancelled: %w", err)
			}
		}

		// First, get the device ID by asset tag
		resp, err := c.apiClient.DevicesClient().ListDevices(ctx, &devicetrustv1.ListDevicesRequest{})
		if err != nil {
			if attempt == maxRetries {
				return fmt.Errorf("failed to list devices to find %s after %d attempts: %w", serialNumber, maxRetries+1, err)
			}
			// Continue to retry
		} else {
			// Find the device with matching asset tag
			var deviceID string
			for _, device := range resp.Devices {
				if device.AssetTag == serialNumber {
					deviceID = device.Id
					break
				}
			}

			if deviceID == "" {
				return fmt.Errorf("device with serial number %s not found in Teleport", serialNumber)
			}

			// Delete the device
			_, err = c.apiClient.DevicesClient().DeleteDevice(ctx, &devicetrustv1.DeleteDeviceRequest{
				DeviceId: deviceID,
			})
			if err == nil {
				return nil
			}

			// If this is the last attempt, return the error
			if attempt == maxRetries {
				return fmt.Errorf("failed to delete device %s after %d attempts: %w", serialNumber, maxRetries+1, err)
			}
		}

		// Calculate delay with exponential backoff
		delay := baseDelay * time.Duration(1<<attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	return nil
}

// DeleteDevices deletes multiple devices from Teleport in bulk operations.
func (c *Client) DeleteDevices(ctx context.Context, serialNumbers []string, batchSize int) (*BulkDeleteResult, error) {
	if c.apiClient == nil {
		return nil, fmt.Errorf("Teleport client not connected")
	}

	if len(serialNumbers) == 0 {
		return &BulkDeleteResult{}, nil
	}

	result := &BulkDeleteResult{
		FailedDevices: make([]FailedDevice, 0),
		Errors:        make([]error, 0),
	}

	// Process devices in batches
	for i := 0; i < len(serialNumbers); i += batchSize {
		end := i + batchSize
		if end > len(serialNumbers) {
			end = len(serialNumbers)
		}

		batch := serialNumbers[i:end]
		batchResult := c.processDeleteBatch(ctx, batch)

		// Aggregate results
		result.SuccessCount += batchResult.SuccessCount
		result.FailedDevices = append(result.FailedDevices, batchResult.FailedDevices...)
		result.Errors = append(result.Errors, batchResult.Errors...)
	}

	return result, nil
}

// processDeleteBatch processes a single batch of device deletions.
func (c *Client) processDeleteBatch(ctx context.Context, serialNumbers []string) *BulkDeleteResult {
	result := &BulkDeleteResult{
		FailedDevices: make([]FailedDevice, 0),
		Errors:        make([]error, 0),
	}

	// Apply rate limiting
	if c.rateLimiter != nil {
		if err := c.rateLimiter.WaitForTeleport(ctx); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("rate limiter cancelled: %w", err))
			return result
		}
	}

	// Get all devices to map serial numbers to device IDs
	resp, err := c.apiClient.DevicesClient().ListDevices(ctx, &devicetrustv1.ListDevicesRequest{})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("failed to list devices for deletion: %w", err))
		return result
	}

	// Create a map of asset tags to device IDs
	deviceMap := make(map[string]string)
	for _, device := range resp.Devices {
		if device.AssetTag != "" {
			deviceMap[device.AssetTag] = device.Id
		}
	}

	// Delete each device individually (Teleport doesn't have bulk delete)
	for _, serialNumber := range serialNumbers {
		deviceID, exists := deviceMap[serialNumber]
		if !exists {
			result.FailedDevices = append(result.FailedDevices, FailedDevice{
				SerialNumber: serialNumber,
				Error:        fmt.Errorf("device not found in Teleport"),
			})
			continue
		}

		// Apply rate limiting for each deletion
		if c.rateLimiter != nil {
			if err := c.rateLimiter.WaitForTeleport(ctx); err != nil {
				result.FailedDevices = append(result.FailedDevices, FailedDevice{
					SerialNumber: serialNumber,
					Error:        fmt.Errorf("rate limiter cancelled: %w", err),
				})
				continue
			}
		}

		_, err := c.apiClient.DevicesClient().DeleteDevice(ctx, &devicetrustv1.DeleteDeviceRequest{
			DeviceId: deviceID,
		})
		if err != nil {
			result.FailedDevices = append(result.FailedDevices, FailedDevice{
				SerialNumber: serialNumber,
				Error:        err,
			})
		} else {
			result.SuccessCount++
		}
	}

	return result
}

// AddDevice adds a single device (legacy method for backward compatibility).
// Deprecated: Use AddDevices for better performance.
func (c *Client) AddDevice(ctx context.Context, serialNumber string) error {
	// Create a single device and use bulk method
	device := kandji.Device{
		SerialNumber: serialNumber,
		Platform:     "Mac", // Default to Mac for backward compatibility
	}

	result, err := c.AddDevices(ctx, []kandji.Device{device}, 1)
	if err != nil {
		return err
	}

	if len(result.FailedDevices) > 0 {
		return result.FailedDevices[0].Error
	}

	if len(result.Errors) > 0 {
		return result.Errors[0]
	}

	return nil
}

// UpdateRateLimiter updates the rate limiter used by this client
func (c *Client) UpdateRateLimiter(rateLimiter *ratelimit.Limiter) {
	c.rateLimiter = rateLimiter
}

// GetHealthStatus returns the current health status
func (c *Client) GetHealthStatus() *HealthStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &HealthStatus{
		TeleportConnected: c.healthStatus.TeleportConnected,
		IdentityValid:     c.healthStatus.IdentityValid,
		LastRefresh:       c.healthStatus.LastRefresh,
	}
}

// RefreshIdentity refreshes the identity file and reconnects with 3 retry attempts
func (c *Client) RefreshIdentity(ctx context.Context, cfg config.TeleportConfig) error {
	const maxRetries = 3
	baseDelay := 1 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Load new identity file
		creds := client.LoadIdentityFile(cfg.IdentityFile)

		// Create test client with new credentials
		testClientConfig := client.Config{
			Addrs:       []string{cfg.ProxyAddr},
			Credentials: []client.Credentials{creds},
		}

		// Test the new identity by creating a client and attempting a simple operation
		testClient, err := client.New(ctx, testClientConfig)
		if err != nil {
			if attempt == maxRetries {
				c.mu.Lock()
				c.healthStatus.TeleportConnected = false
				c.healthStatus.IdentityValid = false
				c.mu.Unlock()
				return fmt.Errorf("failed to refresh identity after %d attempts: %w", maxRetries, err)
			}

			// Wait before retrying with exponential backoff
			delay := baseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}

		// Validate the new connection by attempting to list devices
		_, err = testClient.DevicesClient().ListDevices(ctx, &devicetrustv1.ListDevicesRequest{})
		if err != nil {
			testClient.Close()
			if attempt == maxRetries {
				c.mu.Lock()
				c.healthStatus.TeleportConnected = false
				c.healthStatus.IdentityValid = false
				c.mu.Unlock()
				return fmt.Errorf("failed to validate new identity after %d attempts: %w", maxRetries, err)
			}

			// Wait before retrying with exponential backoff
			delay := baseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				continue
			}
		}

		// Success! Replace the current client with the new one
		c.mu.Lock()
		if c.apiClient != nil {
			c.apiClient.Close()
		}
		c.apiClient = testClient
		c.healthStatus.TeleportConnected = true
		c.healthStatus.IdentityValid = true
		c.healthStatus.LastRefresh = time.Now()
		c.mu.Unlock()

		return nil
	}

	// This should never be reached due to the loop structure, but included for safety
	return fmt.Errorf("failed to refresh identity after %d attempts", maxRetries)
}
