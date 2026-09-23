package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/selfupdate"
	"github.com/spf13/cobra"
)

// downloadBudget is the outer cap on fetching a release tarball. It is
// deliberately generous: the transfer is really bounded by selfupdate's stall
// detector (bytes must keep arriving), and this only stops a pathological run
// from lasting forever. A single 60s budget covering check + download was the
// old behaviour, and on a shaped link it failed every time with nothing but
// "context deadline exceeded".
const downloadBudget = 30 * time.Minute

// Proxy credentials are also read from the environment, because anything on the
// command line is visible to every user on the box via `ps`.
const (
	envProxy = "EMX_PROXY"
	envUser  = "EMX_PROXY_USER"
	envPass  = "EMX_PROXY_PASS"
)

func updateCmd() *cobra.Command {
	var checkOnly, noRestart bool
	var proxy, proxyUser, proxyPass string
	c := &cobra.Command{
		Use:   "update",
		Short: "check for a newer release and self-update the binary",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := ensureSharedScope(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			proxy = firstNonEmpty(proxy, os.Getenv(envProxy))
			proxyUser = firstNonEmpty(proxyUser, os.Getenv(envUser))
			proxyPass = firstNonEmpty(proxyPass, os.Getenv(envPass))
			if proxy == "" && (proxyUser != "" || proxyPass != "") {
				// Credentials alone are not an error — HTTPS_PROXY may be set —
				// but silently authenticating to nothing would be.
				if os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" &&
					os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
					return fmt.Errorf("--proxy-user/--proxy-pass need a proxy: pass --proxy HOST:PORT or set %s", envProxy)
				}
			}
			// A --proxy that isn't a HOST:PORT or URL names one of your own
			// inbounds; look up its port and credentials instead of making the
			// user retype them.
			if proxy != "" && !looksLikeAddress(proxy) {
				addr, u, p, err := resolveInboundProxy(cmd, proxy)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "proxying through inbound %q (%s)\n", proxy, addr)
				proxy = addr
				proxyUser = firstNonEmpty(proxyUser, u)
				proxyPass = firstNonEmpty(proxyPass, p)
			}
			hc, err := selfupdate.NewClient(proxy, proxyUser, proxyPass)
			if err != nil {
				return err
			}

			// Latest carries its own short cap; this ctx only honours Ctrl-C.
			rel, err := selfupdate.Latest(cmd.Context(), hc)
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}
			if !selfupdate.Newer(version, rel.Tag) {
				fmt.Fprintf(out, "up to date (%s)\n", version)
				if checkOnly || noRestart {
					return nil
				}
				// Nothing to download, but the runtime may still lag the
				// binary: a daemon from before a manual install, or none at all.
				switch {
				case daemonVersionDiffers(cmd, version):
					fmt.Fprintln(out, "running daemon is older; restarting it on the installed version…")
				case !daemonPresent():
					fmt.Fprintln(out, "daemon is not running; starting it…")
				default:
					return nil
				}
				return finishUpdate(cmd, version)
			}
			fmt.Fprintf(out, "update available: %s → %s\n", version, rel.Tag)
			if checkOnly {
				fmt.Fprintln(out, "run `emx update` to install")
				return nil
			}

			url, ok := rel.AssetURL(runtime.GOOS, runtime.GOARCH)
			if !ok {
				return fmt.Errorf("no %s/%s build in release %s", runtime.GOOS, runtime.GOARCH, rel.Tag)
			}
			dctx, cancel := context.WithTimeout(cmd.Context(), downloadBudget)
			defer cancel()
			fmt.Fprintln(out, "downloading…")
			if err := selfupdate.Apply(dctx, hc, url, progressLine(out)); err != nil {
				return fmt.Errorf("download %s: %w", rel.Tag, err)
			}
			fmt.Fprintf(out, "\nupdated to %s\n", rel.Tag)

			if noRestart {
				fmt.Fprintln(out, "restart the daemon to run the new version:  emx restart")
				return nil
			}
			return finishUpdate(cmd, rel.Tag)
		},
	}
	c.Flags().BoolVar(&checkOnly, "check", false, "only report whether an update is available")
	c.Flags().BoolVar(&noRestart, "no-restart", false, "don't restart the daemon after updating")
	c.Flags().StringVar(&proxy, "proxy", "",
		"fetch the release through a proxy when GitHub is unreachable from this box: the `NAME` of one of "+
			"your socks inbounds (port and credentials are looked up for you), a HOST:PORT, or a full "+
			"http:// / socks5h:// URL ["+envProxy+"]")
	c.Flags().StringVar(&proxyUser, "proxy-user", "", "`username` for a proxy that requires auth ["+envUser+"]")
	c.Flags().StringVar(&proxyPass, "proxy-pass", "",
		"`password` for --proxy-user; prefer "+envPass+" in the environment, since a flag is visible in `ps`")
	return c
}

// daemonVersionDiffers catches a manual/global binary replacement that left
// the old process serving stale templates and RPC behaviour.
func daemonVersionDiffers(cmd *cobra.Command, want string) bool {
	if want == "" || want == "dev" {
		return false
	}
	if !daemonPresent() {
		return false
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	client, conn, err := dialReady(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	reply, err := client.Ping(ctx, &emxv1.PingRequest{})
	return err == nil && reply.Version != "" && reply.Version != want
}

// looksLikeAddress reports whether spec is already a proxy address (a URL, or a
// HOST:PORT) rather than the name of an inbound to resolve. Only a numeric port
// after the last colon counts, so an inbound named `eu:backup` is still a name.
func looksLikeAddress(spec string) bool {
	if strings.Contains(spec, "://") {
		return true
	}
	_, port, err := net.SplitHostPort(spec)
	if err != nil {
		return false
	}
	_, err = strconv.Atoi(port)
	return err == nil
}

// resolveInboundProxy turns an inbound name into the loopback address and socks
// credentials to reach it. The inbound has to be one this box can actually use
// as a proxy: enabled, socks, and egressing somewhere other than this box.
func resolveInboundProxy(cmd *cobra.Command, name string) (addr, user, pass string, err error) {
	err = withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
		list, err := cl.InboundList(ctx, &emxv1.Empty{})
		if err != nil {
			return err
		}
		var match *emxv1.InboundInfo
		for _, in := range list.Inbounds {
			if strings.EqualFold(in.Name, name) {
				match = in
				break
			}
		}
		if match == nil {
			return fmt.Errorf("--proxy %q is neither a HOST:PORT nor an inbound name (`emx in ls` to see them)", name)
		}
		if match.Protocol != "socks" {
			return fmt.Errorf("inbound %q is %s; --proxy needs a socks inbound to dial through", match.Name, match.Protocol)
		}
		if !match.Enabled {
			return fmt.Errorf("inbound %q is disabled", match.Name)
		}

		cfg, err := cl.InboundGetConfig(ctx, &emxv1.IdRequest{Id: match.Id})
		if err != nil {
			return err
		}
		var full xray.Inbound
		if err := json.Unmarshal([]byte(cfg.Json), &full); err != nil {
			return fmt.Errorf("read inbound %q: %w", match.Name, err)
		}
		if kind, _, perr := xray.ParseTarget(full.Target); perr == nil && kind == xray.TargetDirect {
			return fmt.Errorf("inbound %q targets direct — it egresses from this same box, so it can't reach what this box can't; pick one aimed at a master or an entry", match.Name)
		}

		// Whatever the inbound advertises, we reach it over loopback.
		host := full.Listen
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, strconv.Itoa(full.Port))
		user, pass = full.SocksUser, full.Password
		return nil
	})
	return addr, user, pass, err
}

// progressLine returns a progress callback that redraws a single line, so a
// long download shows movement instead of looking wedged.
func progressLine(out io.Writer) func(done, total int64) {
	return func(done, total int64) {
		if total > 0 {
			fmt.Fprintf(out, "\r  %s / %s (%d%%)   ", human(done), human(total), done*100/total)
			return
		}
		fmt.Fprintf(out, "\r  %s   ", human(done))
	}
}

// human formats a byte count in binary units.
func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// finishUpdate brings everything onto the installed binary with no manual
// step: refresh the systemd unit if it is stale, stop leftovers from the old
// build, (re)start the daemon — also when it was not running — and wait until
// the new daemon answers with the new version and its xray is up.
func finishUpdate(cmd *cobra.Command, want string) error {
	out := cmd.OutOrStdout()
	if err := refreshSystemdUnit(out); err != nil {
		fmt.Fprintf(out, "warning: %v\n", err)
	}
	fmt.Fprintln(out, "restarting daemon…")
	if err := restartDaemon(cmd); err != nil {
		return fmt.Errorf("binary installed, but the daemon did not start: %w (see `emx xray logs`)", err)
	}
	return verifyRuntime(cmd, want)
}

// verifyRuntime waits for the daemon to report version want and for xray to
// run, then prints one line per component. Failing loudly here is the point:
// an update that "succeeded" onto a daemon that isn't serving is worse than
// an error.
func verifyRuntime(cmd *cobra.Command, want string) error {
	out := cmd.OutOrStdout()
	deadline := time.Now().Add(20 * time.Second)
	var st *emxv1.StatusReply
	var got string
	for {
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		if c, conn, err := dialReady(ctx); err == nil {
			if p, err := c.Ping(ctx, &emxv1.PingRequest{}); err == nil {
				got = p.Version
			}
			st, _ = c.Status(ctx, &emxv1.StatusRequest{})
			conn.Close()
		}
		cancel()
		versionOK := sameVersion(got, want)
		xrayUp := st != nil && st.Xray != nil && st.Xray.Running
		if versionOK && (xrayUp || !hasEnabledInbound(cmd)) {
			break
		}
		if time.Now().After(deadline) {
			if !versionOK {
				return fmt.Errorf("the daemon is running %q, not %s — run `emx restart`", got, want)
			}
			msg := "xray did not start"
			if st != nil && st.Xray != nil {
				if st.Xray.ConfigError != "" {
					msg += ": " + st.Xray.ConfigError
				} else if st.Xray.LastError != "" {
					msg += ": " + st.Xray.LastError
				}
			}
			return fmt.Errorf("daemon %s is up, but %s (see `emx xray logs`)", got, msg)
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(out, "daemon:  %s (pid %d)\n", got, st.DaemonPid)
	if st.Xray != nil && st.Xray.Running {
		fmt.Fprintf(out, "xray:    running (pid %d)\n", st.Xray.Pid)
	} else {
		fmt.Fprintln(out, "xray:    idle (no enabled inbound to serve)")
	}
	if st.Xray != nil && st.Xray.ConfigError != "" {
		fmt.Fprintf(out, "config:  %s\n", st.Xray.ConfigError)
	}
	return nil
}

// sameVersion compares release tags, ignoring a leading "v". A "dev" build
// can't be told apart, so it passes.
func sameVersion(got, want string) bool {
	if want == "" || want == "dev" {
		return got != ""
	}
	return strings.TrimPrefix(got, "v") == strings.TrimPrefix(want, "v")
}

// hasEnabledInbound reports whether xray has anything to serve; without one
// the daemon intentionally keeps xray down.
func hasEnabledInbound(cmd *cobra.Command) bool {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	c, conn, err := dialReady(ctx)
	if err != nil {
		return true // can't tell: keep waiting for xray
	}
	defer conn.Close()
	list, err := c.InboundList(ctx, &emxv1.Empty{})
	if err != nil {
		return true
	}
	for _, in := range list.Inbounds {
		if in.Enabled {
			return true
		}
	}
	return false
}
