package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
				fmt.Fprintf(out, "xray:    running (pid %d, restarts %d)\n", x.Pid, x.Restarts)
			} else {
				le := ""
				if x != nil && x.LastError != "" {
					le = " — " + x.LastError
				}
				fmt.Fprintf(out, "xray:    stopped%s\n", le)
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
