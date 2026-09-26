package estim

import "context"

// HeartbeatForTest runs one heartbeat tick.
func (s *Session) HeartbeatForTest(ctx context.Context) { s.heartbeat(ctx) }

// TelemetryForTest runs one telemetry batch tick.
func (s *Session) TelemetryForTest(ctx context.Context) { s.telemetryTick() }
