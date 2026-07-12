package daemon

import (
	"log"
	"os/exec"
	"sync"
	"time"
)

const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
	stableRun  = 60 * time.Second // a child that ran this long resets the backoff
)

// CmdFactory builds a fresh xray child command each (re)start. Returning an
// error causes the watchdog to back off and retry.
type CmdFactory func() (*exec.Cmd, error)

// Watchdog supervises the xray child: it (re)spawns the process, restarts it on
// unexpected exit with exponential backoff, and exposes health for Status. A
// clean Stop() suppresses the restart. This is separate from zero-restart member
// churn (SyncDialerMembers) — the watchdog only fires on an actual process exit.
type Watchdog struct {
	factory CmdFactory
	log     *log.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	restarts int32
	lastErr  string

	stopCh chan struct{}
	doneCh chan struct{}
}

func NewWatchdog(factory CmdFactory, logger *log.Logger) *Watchdog {
	return &Watchdog{factory: factory, log: logger}
}

// Start launches the supervise loop. Non-blocking.
func (w *Watchdog) Start() {
	w.mu.Lock()
	if w.stopCh != nil {
		w.mu.Unlock()
		return // already started
	}
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	w.mu.Unlock()
	go w.supervise()
}

func (w *Watchdog) supervise() {
	// On exit, clear stopCh so a future Start() can relaunch the loop (Reconcile
	// stops xray when no entries are routable, then restarts it when some are).
	defer func() {
		w.mu.Lock()
		w.stopCh = nil
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
	}()
	backoff := backoffMin
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		cmd, err := w.factory()
		if err != nil {
			w.setErr("build xray command: " + err.Error())
			if !w.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if err := cmd.Start(); err != nil {
			w.setErr("start xray: " + err.Error())
			if !w.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		w.mu.Lock()
		w.cmd = cmd
		w.running = true
		w.lastErr = ""
		w.mu.Unlock()
		w.log.Printf("xray started (pid %d)", cmd.Process.Pid)

		startedAt := time.Now()
		waitErr := cmd.Wait() // blocks until the child exits

		w.mu.Lock()
		w.running = false
		stopping := false
		select {
		case <-w.stopCh:
			stopping = true
		default:
		}
		if !stopping {
			w.restarts++
			if waitErr != nil {
				w.lastErr = waitErr.Error()
			}
		}
		w.mu.Unlock()

		if stopping {
			w.log.Printf("xray stopped (clean)")
			return
		}
		// A run that stayed up past stableRun clears accumulated backoff so a
		// deliberate restart (config reload) or a rare crash respawns promptly.
		if time.Since(startedAt) > stableRun {
			backoff = backoffMin
		}
		w.log.Printf("xray exited (%v) — restarting in %s", waitErr, backoff)
		if !w.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// Stop signals the loop to quit and terminates the current child, waiting up to
// grace for a clean SIGTERM before returning.
func (w *Watchdog) Stop(grace time.Duration) {
	w.mu.Lock()
	if w.stopCh == nil {
		w.mu.Unlock()
		return
	}
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	cmd := w.cmd
	done := w.doneCh
	w.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(sigTerm)
		select {
		case <-done:
			return
		case <-time.After(grace):
			_ = cmd.Process.Kill() // hard kill after grace
		}
	}
	<-done
}

// Restart terminates the current child; the supervise loop respawns it, picking
// up a freshly-generated config. No-op if nothing is running yet.
func (w *Watchdog) Restart() {
	w.mu.Lock()
	cmd := w.cmd
	running := w.running
	w.mu.Unlock()
	if running && cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(sigTerm)
	}
}

// IsStarted reports whether the supervise loop is running (Start was called and
// Stop has not).
func (w *Watchdog) IsStarted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopCh == nil {
		return false
	}
	select {
	case <-w.stopCh:
		return false
	default:
		return true
	}
}

// State snapshots watchdog health for the Status RPC.
func (w *Watchdog) State() (running bool, pid int, restarts int32, lastErr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running && w.cmd != nil && w.cmd.Process != nil {
		pid = w.cmd.Process.Pid
	}
	return w.running, pid, w.restarts, w.lastErr
}

func (w *Watchdog) setErr(msg string) {
	w.mu.Lock()
	w.lastErr = msg
	w.mu.Unlock()
	w.log.Print(msg)
}

// sleep waits for d unless a stop is requested first. Returns false if stopping.
func (w *Watchdog) sleep(d time.Duration) bool {
	select {
	case <-w.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	n := cur * 2
	if n > backoffMax {
		return backoffMax
	}
	return n
}
