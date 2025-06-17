package ratelimit

import (
	"context"

	"golang.org/x/time/rate"
)

// Limiter provides rate limiting for different API endpoints
type Limiter struct {
	kandjiLimiter   *rate.Limiter
	teleportLimiter *rate.Limiter
}

// Config holds rate limiting configuration
type Config struct {
	KandjiRequestsPerSecond   float64
	TeleportRequestsPerSecond float64
	BurstCapacity             int
}

// New creates a new rate limiter with the given configuration
func New(cfg Config) *Limiter {
	return &Limiter{
		kandjiLimiter:   rate.NewLimiter(rate.Limit(cfg.KandjiRequestsPerSecond), cfg.BurstCapacity),
		teleportLimiter: rate.NewLimiter(rate.Limit(cfg.TeleportRequestsPerSecond), cfg.BurstCapacity),
	}
}

// WaitForKandji waits for permission to make a Kandji API request
func (l *Limiter) WaitForKandji(ctx context.Context) error {
	return l.kandjiLimiter.Wait(ctx)
}

// WaitForTeleport waits for permission to make a Teleport API request
func (l *Limiter) WaitForTeleport(ctx context.Context) error {
	return l.teleportLimiter.Wait(ctx)
}

// AllowKandji checks if a Kandji API request is allowed without blocking
func (l *Limiter) AllowKandji() bool {
	return l.kandjiLimiter.Allow()
}

// AllowTeleport checks if a Teleport API request is allowed without blocking
func (l *Limiter) AllowTeleport() bool {
	return l.teleportLimiter.Allow()
}
