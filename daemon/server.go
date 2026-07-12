package daemon

import (
	"context"
	"os"
	"sync"
	"time"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/gravisun/em-xray/core/xray"
	"github.com/gravisun/em-xray/internal/selfupdate"
	"github.com/gravisun/em-xray/internal/xraybin"
)

// buildVersion is set by the daemon binary at init via SetVersion so Ping can
// report it without a package cycle back to main.
var buildVersion = "dev"

// SetVersion records the emx build version for the Ping RPC.
func SetVersion(v string) { buildVersion = v }

// Server implements the gRPC Daemon service. It holds the store, supervisor and
// fetcher so later phases can add subscription/entry/winner RPCs.
type Server struct {
	emxv1.UnimplementedDaemonServer
	startTime time.Time
	store     *xray.Store
	sup       *Supervisor
	fetcher   *SubFetcher

	stopOnce sync.Once
	stop     chan struct{}

	pubMu sync.Mutex
	pubIP string // cached detected public IP for share-link hosts

	updMu     sync.Mutex
	latestVer string // newest release tag seen by the periodic update checker
}

// setLatest records the newest release tag the update checker found.
func (s *Server) setLatest(tag string) {
	s.updMu.Lock()
	s.latestVer = tag
	s.updMu.Unlock()
}

func (s *Server) latest() string {
	s.updMu.Lock()
	defer s.updMu.Unlock()
	return s.latestVer
}

func (s *Server) Ping(context.Context, *emxv1.PingRequest) (*emxv1.PingReply, error) {
	return &emxv1.PingReply{Version: buildVersion, XrayVersion: xraybin.Version()}, nil
}

func (s *Server) Status(context.Context, *emxv1.StatusRequest) (*emxv1.StatusReply, error) {
	running, pid, restarts, lastErr := s.sup.XrayState()
	latest := s.latest()
	return &emxv1.StatusReply{
		DaemonPid: int32(os.Getpid()),
		UptimeSec: int64(time.Since(s.startTime).Seconds()),
		Xray: &emxv1.XrayState{
			Running:   running,
			Pid:       int32(pid),
			Restarts:  restarts,
			LastError: lastErr,
		},
		LatestVersion:   latest,
		UpdateAvailable: latest != "" && selfupdate.Newer(buildVersion, latest),
	}, nil
}

func (s *Server) Shutdown(context.Context, *emxv1.ShutdownRequest) (*emxv1.ShutdownReply, error) {
	s.stopOnce.Do(func() { close(s.stop) })
	return &emxv1.ShutdownReply{}, nil
}
