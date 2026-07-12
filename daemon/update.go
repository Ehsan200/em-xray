package daemon

import (
	"context"
	"log"
	"time"

	"github.com/gravisun/em-xray/internal/selfupdate"
)

// updateCheckInterval is how often the daemon polls GitHub for a newer release.
const updateCheckInterval = 6 * time.Hour

// updateChecker periodically records the latest release tag so `emx status`
// (and the TUI) can show that an update is available. It never installs
// anything — the user runs `emx update` for that.
func updateChecker(ctx context.Context, srv *Server, logger *log.Logger) {
	check := func() {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		rel, err := selfupdate.Latest(cctx)
		if err != nil {
			return // offline / rate-limited — try again next tick
		}
		srv.setLatest(rel.Tag)
		if selfupdate.Newer(buildVersion, rel.Tag) {
			logger.Printf("update available: %s → %s (run `emx update`)", buildVersion, rel.Tag)
		}
	}
	check() // one at startup
	t := time.NewTicker(updateCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
