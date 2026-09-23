package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/daemon"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/spf13/cobra"
)

func startCmd() *cobra.Command {
	var foreground bool
	c := &cobra.Command{
		Use:   "start",
		Short: "start the daemon in the background (auto-restarts xray on crash)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := ensureSharedScope(); err != nil {
				return err
			}
			if foreground {
				return daemon.Run(cmd.Context())
			}
			return startDetached(cmd)
		},
	}
	c.Flags().BoolVar(&foreground, "foreground", false, "run in the foreground instead of detaching")
	return c
}

// startDetached forks a fully detached copy of emx running `daemon run`, then
// waits for it to answer on the socket. Idempotent: a live daemon short-circuits.
func startDetached(cmd *cobra.Command) error {
	p := paths.Default()
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	if pid, alive := daemon.RunningPID(p); alive {
		fmt.Fprintf(cmd.OutOrStdout(), "already running (pid %d)\n", pid)
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(p.ErrorLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	child := exec.Command(exe, "daemon", "run")
	child.Stdin = nil
	child.Stdout = logf
	child.Stderr = logf
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // new session: survives terminal close
	if err := child.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	_ = child.Process.Release()

	if err := waitReady(5 * time.Second); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "daemon started")
	return nil
}

func daemonRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "run" {
				return fmt.Errorf("unknown daemon subcommand %q", args[0])
			}
			if err := ensureSharedScope(); err != nil {
				return err
			}
			return daemon.Run(cmd.Context())
		},
	}
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "stop the background daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := paths.Default()
			pid, alive := daemon.RunningPID(p)
			if !alive {
				fmt.Fprintln(cmd.OutOrStdout(), "not running")
				return nil
			}
			// Prefer a graceful RPC shutdown; fall back to SIGTERM.
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
			defer cancel()
			if c, conn, err := dialReady(ctx); err == nil {
				_, _ = c.Shutdown(ctx, &emxv1.ShutdownRequest{})
				conn.Close()
			} else if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
			if err := waitGone(p, 5*time.Second); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon stopped")
			return nil
		},
	}
}

func restartCmd() *cobra.Command {
	var xrayOnly bool
	c := &cobra.Command{
		Use:   "restart",
		Short: "restart the background daemon (or just the xray child with --xray)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if xrayOnly {
				return restartXray(cmd)
			}
			return restartDaemon(cmd)
		},
	}
	c.Flags().BoolVar(&xrayOnly, "xray", false, "only cycle the xray child (keeps the daemon and its socket)")
	return c
}

// restartXray asks the running daemon to regenerate config.json and force-cycle
// the xray child. Cheaper than a daemon restart and enough to clear a wedged
// xray (stuck balancer, listeners that stopped answering).
func restartXray(cmd *cobra.Command) error {
	return withClientTimeout(cmd, 30*time.Second, func(ctx context.Context, cl emxv1.DaemonClient) error {
		reply, err := cl.XrayRestart(ctx, &emxv1.Empty{})
		if err != nil {
			return err
		}
		if reply.Running {
			fmt.Fprintf(cmd.OutOrStdout(), "%s (pid %d)\n", reply.Message, reply.Pid)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), reply.Message)
		}
		return nil
	})
}

// restartDaemon stops the running daemon (graceful RPC, then re-spawns). Spawns
// fresh even if it wasn't running. Used by `emx restart` and by `emx update`
// after swapping the binary so the new code takes effect.
func restartDaemon(cmd *cobra.Command) error {
	// An upgrade can move the runtime directory, so a daemon started by the
	// previous build is invisible to the pid file but still owns an xray child.
	stopStaleProcesses(cmd)
	// Keep a systemd-managed daemon under systemd — including one whose unit
	// is installed but stopped or failed: `systemctl restart` starts it, where
	// spawning a detached daemon would leave an unmanaged one beside the unit.
	// (A graceful RPC shutdown counts as success, so Restart=on-failure would
	// otherwise leave the unit inactive.)
	if active := systemdDaemonActive(); active || systemdUnitInstalled() {
		system := os.Geteuid() == 0
		restart := systemctl(system, "restart", systemdUnitName)
		restart.Stdout, restart.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
		err := restart.Run()
		if err == nil {
			return waitReady(15 * time.Second)
		}
		if active {
			return fmt.Errorf("restart %s: %w", systemdUnitName, err)
		}
		// An installed but unusable unit (e.g. no user systemd session):
		// fall through and run the daemon ourselves rather than leave it down.
		fmt.Fprintf(cmd.OutOrStdout(), "systemctl restart %s failed (%v); starting the daemon directly\n", systemdUnitName, err)
	}
	p := paths.Default()
	if _, alive := daemon.RunningPID(p); alive {
		if c, conn, err := dialReady(cmd.Context()); err == nil {
			_, _ = c.Shutdown(cmd.Context(), &emxv1.ShutdownRequest{})
			conn.Close()
		}
		_ = waitGone(p, 5*time.Second)
	}
	return startDetached(cmd)
}

// systemdDaemonActive reports whether this scope's emx unit is running, which
// makes systemd — not emx — responsible for respawning the daemon.
func systemdDaemonActive() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return systemctl(os.Geteuid() == 0, "is-active", "--quiet", systemdUnitName).Run() == nil
}

// stopStaleProcesses clears everything a previous daemon may have left behind:
// a daemon from an older runtime layout, and any orphaned xray still holding
// the inbound ports. Both would otherwise keep serving an old config alongside
// the new one, since the listeners share the port.
func stopStaleProcesses(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	if dir, pid, ok := daemon.LegacyDaemon(paths.Default()); ok {
		fmt.Fprintf(out, "stopping daemon from the previous layout (pid %d, %s)\n", pid, dir)
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	if reaped := daemon.ReapStrayXray(paths.Default(), nil, currentXrayPID(cmd)); len(reaped) > 0 {
		fmt.Fprintf(out, "stopped %d orphaned xray process(es): %v\n", len(reaped), reaped)
	}
}

// currentXrayPID is the xray the live daemon owns, so cleanup never kills the
// child of a daemon that is still doing its job. 0 when nothing answers.
func currentXrayPID(cmd *cobra.Command) int {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	c, conn, err := dialReady(ctx)
	if err != nil {
		return 0
	}
	defer conn.Close()
	st, err := c.Status(ctx, &emxv1.StatusRequest{})
	if err != nil || st.Xray == nil {
		return 0
	}
	return int(st.Xray.Pid)
}

// daemonPresent reports whether any emx daemon is running for this scope: the
// current layout, a previous one, or a systemd unit whose pid file this build
// does not look at.
func daemonPresent() bool {
	if _, alive := daemon.RunningPID(paths.Default()); alive {
		return true
	}
	if _, _, ok := daemon.LegacyDaemon(paths.Default()); ok {
		return true
	}
	return systemdDaemonActive()
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show daemon + xray health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			c, conn, err := dialReady(ctx)
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon: stopped")
				return nil
			}
			defer conn.Close()
			st, err := c.Status(ctx, &emxv1.StatusRequest{})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "daemon:  running (pid %d, up %ds)\n", st.DaemonPid, st.UptimeSec)
			x := st.Xray
			if x != nil && x.Running {
				fmt.Fprintf(out, "xray:    running (pid %d, crash restarts %d)\n", x.Pid, x.Restarts)
				fmt.Fprintf(out, "config:  %d changes applied live, %d needed a restart\n", x.LiveApplies, x.ConfigRestarts)
				if x.ConfigError != "" {
					fmt.Fprintf(out, "config:  LAST CHANGE REFUSED — %s\n", x.ConfigError)
				}
				if x.RejectedNodes > 0 {
					fmt.Fprintf(out, "nodes:   %d pool node(s) refused by xray, left out (emx sub nodes <id> says why)\n", x.RejectedNodes)
				}
				if x.ParkedNodes > 0 {
					fmt.Fprintf(out, "parked:  %d dead pool node(s) left out until retry (emx sub nodes <id>)\n", x.ParkedNodes)
				}
				switch {
				case !x.HealthChecked:
					fmt.Fprintln(out, "health:  waiting for first responsiveness check")
				case x.Responsive:
					fmt.Fprintf(out, "health:  responsive (automatic health restarts %d)\n", x.HealthRestarts)
				default:
					fmt.Fprintf(out, "health:  degraded — %s\n", x.HealthMessage)
				}
			} else {
				le := ""
				if x != nil && x.LastError != "" {
					le = " — " + x.LastError
				}
				fmt.Fprintf(out, "xray:    stopped%s\n", le)
			}
			if _, err := exec.LookPath("caddy"); err == nil {
				active := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "caddy").Run() == nil
				enabled := exec.CommandContext(ctx, "systemctl", "is-enabled", "--quiet", "caddy").Run() == nil
				fmt.Fprintf(out, "caddy:   installed (active=%v, enabled=%v)\n", active, enabled)
			} else {
				fmt.Fprintln(out, "caddy:   not installed")
			}
			if st.UpdateAvailable {
				fmt.Fprintf(out, "update:  %s available (run `emx update`)\n", st.LatestVersion)
			}
			// Pool health. A master whose pool is empty now fails CLOSED rather
			// than leaking this box's IP, so "nothing works" and "pool is empty"
			// look identical from the outside — say which it is.
			if x != nil && x.Running {
				if w, err := c.Winners(ctx, &emxv1.Empty{}); err == nil && len(w.Winners) > 0 {
					fmt.Fprintln(out, "pools:")
					for _, m := range w.Winners {
						switch {
						case m.Members == 0:
							fmt.Fprintf(out, "  %s: BLOCKED — pool empty (refresh its subscription or check its dialer)\n", m.Master)
						case m.Node == "":
							fmt.Fprintf(out, "  %s: %d members, selecting… (no probe result yet — traffic blocked until one lands)\n", m.Master, m.Members)
						case m.Alive == 0:
							fmt.Fprintf(out, "  %s: %d members, NONE answering pings — this box's uplink or the whole pool is down (traffic falls back to the first member)\n", m.Master, m.Members)
						case len(m.Nodes) > 0:
							fmt.Fprintf(out, "  %s: %s alive, via %s\n", m.Master, aliveLabel(m), strings.Join(m.Nodes, ", "))
						default:
							fmt.Fprintf(out, "  %s: %d members, via %s\n", m.Master, m.Members, m.Node)
						}
					}
				}
			}
			return nil
		},
	}
}

// waitGone blocks until the daemon pid is no longer alive or the timeout hits.
func waitGone(p paths.Paths, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, alive := daemon.RunningPID(p); !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon still running after %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
