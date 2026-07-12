package daemon

import (
	"context"
	"fmt"
	"os"

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
