package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Config holds all configuration for the application.
type Config struct {
	SyncInterval time.Duration   `yaml:"sync_interval"`
	OnMissing    string          `yaml:"on_missing"`
	Kandji       KandjiConfig    `yaml:"kandji"`
	Teleport     TeleportConfig  `yaml:"teleport"`
	RateLimits   RateLimitConfig `yaml:"rate_limits"`
	Batch        BatchConfig     `yaml:"batch"`
	Log          LoggingConfig   `yaml:"log"`
}

type BlueprintFilter struct {
	BlueprintIDs   []string `yaml:"blueprint_ids"`
	BlueprintNames []string `yaml:"blueprint_names"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type KandjiConfig struct {
	ApiURL                   string          `yaml:"api_url"`
	ApiToken                 string          `yaml:"api_token"`
	SyncDevicesWithoutOwners bool            `yaml:"sync_devices_without_owners"`
	SyncMobileDevices        bool            `yaml:"sync_mobile_devices"`
	IncludeTags              []string        `yaml:"include_tags"`
	ExcludeTags              []string        `yaml:"exclude_tags"`
	BlueprintsInclude        BlueprintFilter `yaml:"blueprints_include"`
	BlueprintsExclude        BlueprintFilter `yaml:"blueprints_exclude"`
}

// TeleportConfig holds Teleport-specific settings.
type TeleportConfig struct {
	ProxyAddr    string `yaml:"proxy_addr"`
	IdentityFile string `yaml:"identity_file"`
}

// RateLimitConfig holds rate limiting settings.
type RateLimitConfig struct {
	KandjiRequestsPerSecond   float64 `yaml:"kandji_requests_per_second"`
	TeleportRequestsPerSecond float64 `yaml:"teleport_requests_per_second"`
	BurstCapacity             int     `yaml:"burst_capacity"`
}

// BatchConfig holds batch processing settings.
type BatchConfig struct {
	Size                 int `yaml:"size"`
	MaxConcurrentBatches int `yaml:"max_concurrent_batches"`
}

// LoadConfig reads configuration from file or environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{}

	// Load from config file if it exists
	if _, err := os.Stat("config.yaml"); err == nil {
		data, err := os.ReadFile("config.yaml")
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}

		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}

	// Override with environment variables if set
	if url := os.Getenv("KANDJI_API_URL"); url != "" {
		cfg.Kandji.ApiURL = url
	}
	if token := os.Getenv("KANDJI_API_TOKEN"); token != "" {
		cfg.Kandji.ApiToken = token
	}
	if addr := os.Getenv("TELEPORT_PROXY_ADDR"); addr != "" {
		cfg.Teleport.ProxyAddr = addr
	}
	if file := os.Getenv("TELEPORT_IDENTITY_FILE"); file != "" {
		cfg.Teleport.IdentityFile = file
	}
	if onMissing := os.Getenv("ON_MISSING"); onMissing != "" {
		cfg.OnMissing = onMissing
	}
	if syncWithoutOwners := os.Getenv("SYNC_DEVICES_WITHOUT_OWNERS"); syncWithoutOwners != "" {
		cfg.Kandji.SyncDevicesWithoutOwners = strings.ToLower(syncWithoutOwners) == "true"
	}

	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		cfg.Log.Level = logLevel
	}

	// Set default sync interval if not specified
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = 5 * time.Minute
	}

	// Set default on_missing behavior if not specified
	if cfg.OnMissing == "" {
		cfg.OnMissing = "ignore"
	}

	// Set default rate limits if not specified
	if cfg.RateLimits.KandjiRequestsPerSecond == 0 {
		cfg.RateLimits.KandjiRequestsPerSecond = 10.0
	}
	if cfg.RateLimits.TeleportRequestsPerSecond == 0 {
		cfg.RateLimits.TeleportRequestsPerSecond = 5.0
	}
	if cfg.RateLimits.BurstCapacity == 0 {
		cfg.RateLimits.BurstCapacity = 5
	}

	// Set default batch settings if not specified
	if cfg.Batch.Size == 0 {
		cfg.Batch.Size = 50
	}
	if cfg.Batch.MaxConcurrentBatches == 0 {
		cfg.Batch.MaxConcurrentBatches = 3
	}

	// Validate required configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// Validate checks that all required configuration values are present and valid.
func (c *Config) Validate() error {
	if c.Kandji.ApiURL == "" {
		return fmt.Errorf("KANDJI_API_URL is required")
	}
	if c.Kandji.ApiToken == "" {
		return fmt.Errorf("KANDJI_API_TOKEN is required")
	}
	if c.Teleport.ProxyAddr == "" {
		return fmt.Errorf("TELEPORT_PROXY_ADDR is required")
	}
	if c.Teleport.IdentityFile == "" {
		return fmt.Errorf("TELEPORT_IDENTITY_FILE is required")
	}

	// Validate on_missing values
	validOnMissing := []string{"ignore", "delete", "alert"}
	isValid := false
	for _, valid := range validOnMissing {
		if c.OnMissing == valid {
			isValid = true
			break
		}
	}
	if !isValid {
		return fmt.Errorf("on_missing must be one of: %s", strings.Join(validOnMissing, ", "))
	}

	// Check if identity file exists
	if _, err := os.Stat(c.Teleport.IdentityFile); err != nil {
		return fmt.Errorf("teleport identity file does not exist: %s", c.Teleport.IdentityFile)
	}

	return nil
}
