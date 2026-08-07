package daemon

import (
	"context"
	"fmt"
	"os"
	"strconv"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

// XrayConfig returns the generated xray config.json currently on disk.
func (s *Server) XrayConfig(ctx context.Context, _ *emxv1.Empty) (*emxv1.ConfigReply, error) {
	data, err := os.ReadFile(s.sup.paths.XrayConfig())
	if err != nil {
		return nil, fmt.Errorf("no xray config yet (start the daemon and add an inbound/entry): %w", err)
	}
	return &emxv1.ConfigReply{Json: string(data)}, nil
}

// LogLevel reads (and, when req.Set is non-empty, changes) the xray log level.
// A change persists and triggers a reconcile so xray picks it up.
func (s *Server) LogLevel(ctx context.Context, req *emxv1.LogLevelRequest) (*emxv1.LogLevelReply, error) {
	if req.Set != "" {
		if !xray.ValidLogLevel(req.Set) {
			return nil, fmt.Errorf("invalid log level %q (want %v)", req.Set, xray.LogLevels())
		}
		if err := s.store.SetSetting(xray.SettingLogLevel, req.Set); err != nil {
			return nil, err
		}
		if err := s.sup.Reconcile(); err != nil {
			return nil, fmt.Errorf("saved, but reconcile failed: %w", err)
		}
	}
	return &emxv1.LogLevelReply{Level: s.store.LogLevel()}, nil
}

// LogCap reads (and, when req.Change, sets) the per-file log size cap in MB. The
// rotator reads it live — no reconcile needed. 0 disables rotation.
func (s *Server) LogCap(ctx context.Context, req *emxv1.LogCapRequest) (*emxv1.LogCapReply, error) {
	if req.Change {
		if req.SetMb < 0 {
			return nil, fmt.Errorf("log cap must be >= 0 (0 disables rotation)")
		}
		if err := s.store.SetSetting(xray.SettingLogMaxMB, fmt.Sprintf("%d", req.SetMb)); err != nil {
			return nil, err
		}
	}
	return &emxv1.LogCapReply{Mb: int32(s.store.LogMaxMB())}, nil
}

// ProbeInterval reads (and, when req.Change, sets) the observatory probe
// cadence in seconds. A change persists and reconciles so xray picks it up.
// This is also the fail-closed window: after a restart a master's balancer has
// no observation and blocks until the first probe lands.
func (s *Server) ProbeInterval(ctx context.Context, req *emxv1.ProbeIntervalRequest) (*emxv1.ProbeIntervalReply, error) {
	if req.Change {
		n := int(req.SetSec)
		if n < xray.MinProbeIntervalSec || n > xray.MaxProbeIntervalSec {
			return nil, fmt.Errorf("probe interval must be %d-%d seconds", xray.MinProbeIntervalSec, xray.MaxProbeIntervalSec)
		}
		if err := s.store.SetSetting(xray.SettingProbeInterval, strconv.Itoa(n)); err != nil {
			return nil, err
		}
		if err := s.sup.Reconcile(); err != nil {
			return nil, fmt.Errorf("saved, but reconcile failed: %w", err)
		}
	}
	return &emxv1.ProbeIntervalReply{Sec: int32(s.store.ProbeIntervalSec())}, nil
}

// TrafficRetention reads (and, when req.Change, sets) how many days of hourly
// traffic buckets are kept. Shrinking it prunes on the sampler's next tick.
func (s *Server) TrafficRetention(ctx context.Context, req *emxv1.TrafficRetentionRequest) (*emxv1.TrafficRetentionReply, error) {
	if req.Change {
		if req.SetDays < 1 {
			return nil, fmt.Errorf("retention must be >= 1 day")
		}
		if err := s.store.SetSetting(xray.SettingTrafficDays, fmt.Sprintf("%d", req.SetDays)); err != nil {
			return nil, err
		}
	}
	return &emxv1.TrafficRetentionReply{Days: int32(s.store.TrafficDays())}, nil
}
