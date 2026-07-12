package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/gravisun/em-xray/core/xray"
	"github.com/gravisun/em-xray/internal/paths"
	"google.golang.org/grpc"
)

// sigTerm is the signal used to ask a child (and the daemon) to stop gracefully.
var sigTerm = syscall.SIGTERM

// Run is the daemon entrypoint (invoked by the detached `emx daemon run`). It
// binds the control socket, serves gRPC, supervises the xray child, and blocks
// until stopped by signal, context cancel, or the Shutdown RPC.
func Run(ctx context.Context) error {
	p := paths.Default()
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	if pid, ok := RunningPID(p); ok {
		return fmt.Errorf("daemon already running (pid %d)", pid)
	}
	// Unix socket paths are capped by the kernel (sun_path: 104 on darwin,
	// 108 on linux). A real $XDG_RUNTIME_DIR (/run/user/<uid>) is short; guard
	// so an over-long override fails with a clear message, not bind EINVAL.
	if n := len(p.Socket()); n > 100 {
		return fmt.Errorf("socket path too long (%d bytes): set a shorter $XDG_RUNTIME_DIR: %s", n, p.Socket())
	}
	if err := writePID(p.PIDFile(), os.Getpid()); err != nil {
		return err
	}
	defer os.Remove(p.PIDFile())

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	logger.SetPrefix("[emx] ")

	// Fresh socket: a stale file from a crash would make Listen fail.
	_ = os.Remove(p.Socket())
	lis, err := net.Listen("unix", p.Socket())
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.Socket(), err)
	}
	defer os.Remove(p.Socket())

	// Persistence + supervisor + subscription scheduler.
	store, err := xray.Open(p.DB())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	sup := NewSupervisor(store, p, logger)
	// A successful subscription refresh live-syncs the dialer pool — never a restart.
	fetcher := NewSubFetcher(store, logger, sup.SyncDialerMembers)

	// Bring xray up to match current state (starts the child if any entry is
	// routable). Node churn from the scheduler doesn't reconcile until masters
	// exist (P5/P6), so onChange is nil for now.
	if err := sup.Reconcile(); err != nil {
		logger.Printf("initial reconcile: %v", err)
	}
	fetcher.Start(ctx)

	stop := make(chan struct{})
	srv := &Server{startTime: time.Now(), store: store, sup: sup, fetcher: fetcher, stop: stop}

	gs := grpc.NewServer()
	emxv1.RegisterDaemonServer(gs, srv)
	go func() {
		if err := gs.Serve(lis); err != nil {
			logger.Printf("grpc serve ended: %v", err)
		}
	}()
	logger.Printf("daemon up (pid %d) on %s", os.Getpid(), p.Socket())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
		logger.Print("context cancelled — shutting down")
	case s := <-sig:
		logger.Printf("signal %s — shutting down", s)
	case <-stop:
		logger.Print("shutdown requested — shutting down")
	}

	sup.Stop()
	gs.GracefulStop()
	return nil
}
