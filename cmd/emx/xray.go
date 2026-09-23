package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/daemon"
	"github.com/ehsan200/em-xray/internal/paths"
	"github.com/spf13/cobra"
)

// xrayCmd groups xray introspection: config json, logs, paths.
func xrayCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "xray",
		Short: "inspect the running xray: config json, logs, paths",
	}
	c.AddCommand(xrayConfigCmd(), xrayLogsCmd(), xrayPathsCmd(), xrayLogCapCmd(), xrayRestartCmd(), xrayReapCmd())
	return c
}

// xrayReapCmd cleans up after a daemon that died without stopping its child.
// Those orphans keep the inbound ports open (the listeners share the port), so
// clients intermittently reach an old config.
func xrayReapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reap",
		Short: "stop orphaned xray processes left by a killed daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			keep := currentXrayPID(cmd)
			strays := daemon.ReapStrayXray(paths.Default(), nil, keep)
			if len(strays) == 0 {
				fmt.Fprintln(out, "no orphaned xray processes")
				return nil
			}
			fmt.Fprintf(out, "stopped %d orphaned xray process(es): %v\n", len(strays), strays)
			if keep != 0 {
				fmt.Fprintf(out, "kept the running daemon's xray (pid %d)\n", keep)
			}
			return nil
		},
	}
}

// xrayRestartCmd force-cycles the xray child without touching the daemon.
func xrayRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use: "restart", Short: "regenerate the config and restart the xray child",
		RunE: func(cmd *cobra.Command, _ []string) error { return restartXray(cmd) },
	}
}

func xrayLogCapCmd() *cobra.Command {
	return &cobra.Command{
		Use: "logcap [MB]", Short: "show or set the per-file log size cap (0 disables rotation)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &emxv1.LogCapRequest{}
			if len(args) == 1 {
				mb, err := strconv.Atoi(args[0])
				if err != nil || mb < 0 {
					return fmt.Errorf("bad MB value %q", args[0])
				}
				req.SetMb, req.Change = int32(mb), true
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.LogCap(ctx, req)
				if err != nil {
					return err
				}
				if reply.Mb == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "log rotation: disabled")
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "log cap: %d MB per file (≈%d MB peak with .prev)\n", reply.Mb, reply.Mb*2)
				}
				return nil
			})
		},
	}
}

func xrayConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use: "config", Short: "print the generated xray config.json", Aliases: []string{"json"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.XrayConfig(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), reply.Json)
				return nil
			})
		},
	}
}

func xrayLogsCmd() *cobra.Command {
	var access, follow bool
	var n int
	c := &cobra.Command{
		Use: "logs", Short: "tail xray's error log (or --access), -f to follow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := paths.Default()
			path := p.ErrorLog()
			if access {
				path = p.AccessLog()
			}
			out := cmd.OutOrStdout()
			if err := tailLastLines(path, n, out); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return followFile(ctx, path, out)
		},
	}
	c.Flags().BoolVarP(&access, "access", "a", false, "tail the access log instead of the error log")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines as they arrive")
	c.Flags().IntVarP(&n, "lines", "n", 200, "number of trailing lines to print")
	return c
}

func xrayPathsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "paths", Short: "show the XDG paths em-xray uses",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := paths.Default()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "database    %s\n", p.DB())
			fmt.Fprintf(out, "xray config %s\n", p.XrayConfig())
			fmt.Fprintf(out, "access log  %s\n", p.AccessLog())
			fmt.Fprintf(out, "error log   %s\n", p.ErrorLog())
			fmt.Fprintf(out, "socket      %s\n", p.Socket())
			fmt.Fprintf(out, "assets      %s\n", p.AssetDir())
			return nil
		},
	}
}

// loglevelCmd reads or sets the xray log level (persisted; reconciles on change).
func loglevelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "loglevel [debug|info|warning|error|none]",
		Short: "show or change the xray log level",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := ""
			if len(args) == 1 {
				set = strings.ToLower(args[0])
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.LogLevel(ctx, &emxv1.LogLevelRequest{Set: set})
				if err != nil {
					return err
				}
				if set == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "log level: %s\n", reply.Level)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "log level set to %s\n", reply.Level)
				}
				return nil
			})
		},
	}
}

// probeIntervalCmd exposes the observatory cadence. It is worth surfacing
// because it doubles as the fail-closed window: after an xray restart a master
// has no observation, so its balancer picks nothing and blocks until the first
// probe lands.
func probeIntervalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe-interval [seconds]",
		Short: "show or change how often the observatory pings pool members",
		Long: "The burst observatory health-pings every pool member on this cadence\n" +
			"(default 10s, rolling window of 3, 5s timeout) and each pool's leastLoad\n" +
			"balancer spreads traffic over the best two. A failing node is dropped on\n" +
			"its next ping, so a shorter interval reacts faster; a longer one cuts\n" +
			"probe traffic. Changing it restarts xray.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &emxv1.ProbeIntervalRequest{}
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("bad interval %q (want seconds)", args[0])
				}
				req.SetSec, req.Change = int32(n), true
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.ProbeInterval(ctx, req)
				if err != nil {
					return err
				}
				verb := "probe interval:"
				if req.Change {
					verb = "probe interval set to"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %ds\n", verb, reply.Sec)
				return nil
			})
		},
	}
}

// tailLastLines prints the last n lines of a file (all if it has fewer).
func tailLastLines(path string, n int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "(no log yet at %s)\n", path)
			return nil
		}
		return err
	}
	defer f.Close()
	// Ring buffer of the last n lines.
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	for _, ln := range ring {
		fmt.Fprintln(w, ln)
	}
	return sc.Err()
}

// followFile streams appended bytes until ctx is cancelled, re-opening on
// truncation (log rotation resets the file to 0).
func followFile(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	offset, _ := f.Seek(0, io.SeekCurrent)
	r := bufio.NewReader(f)
	tk := time.NewTicker(300 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(w)
			return nil
		case <-tk.C:
			// Detect truncation (rotation): size shrank below our offset.
			if fi, err := f.Stat(); err == nil && fi.Size() < offset {
				f.Seek(0, io.SeekStart)
				offset = 0
				r.Reset(f)
			}
			for {
				line, err := r.ReadString('\n')
				if len(line) > 0 {
					fmt.Fprint(w, line)
					offset += int64(len(line))
				}
				if err != nil {
					break
				}
			}
		}
	}
}
