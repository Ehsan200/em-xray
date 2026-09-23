package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

const systemdUnitName = "emx.service"

func systemdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "systemd",
		Short: "install a systemd service so the daemon survives reboots/crashes (Linux)",
	}
	c.AddCommand(systemdInstallCmd(), systemdUninstallCmd(), systemdPrintCmd())
	return c
}

// unitPath returns the service file location for a user or system install.
func unitPath(system bool) (string, error) {
	if system {
		return "/etc/systemd/system/" + systemdUnitName, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config/systemd/user", systemdUnitName), nil
}

// genUnit renders the service file. The daemon runs in the foreground so
// systemd supervises it directly (Restart=on-failure covers a daemon crash; the
// daemon's own watchdog still covers the xray child).
func genUnit(exe string, system bool) string {
	wantedBy := "default.target"
	if system {
		wantedBy = "multi-user.target"
	}
	return fmt.Sprintf(`[Unit]
Description=em-xray master dialer daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s start --foreground
Restart=on-failure
RestartSec=3

[Install]
WantedBy=%s
`, exe, wantedBy)
}

func systemctl(system bool, args ...string) *exec.Cmd {
	if !system {
		args = append([]string{"--user"}, args...)
	}
	return exec.Command("systemctl", args...)
}

func systemdInstallCmd() *cobra.Command {
	var system, now bool
	c := &cobra.Command{
		Use:   "install",
		Short: "write + enable the systemd unit (user-level by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("systemd is Linux-only (this is %s); use `emx systemd print` to view the unit", runtime.GOOS)
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			path, err := unitPath(system)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(genUnit(exe, system)), 0o644); err != nil {
				return fmt.Errorf("write unit (need permission for %s?): %w", path, err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote %s\n", path)

			if _, err := exec.LookPath("systemctl"); err != nil {
				fmt.Fprintln(out, "systemctl not found — enable it manually once available")
				return nil
			}
			if err := systemctl(system, "daemon-reload").Run(); err != nil {
				return fmt.Errorf("daemon-reload: %w", err)
			}
			enable := []string{"enable", systemdUnitName}
			if now {
				enable = []string{"enable", "--now", systemdUnitName}
			}
			if err := systemctl(system, enable...).Run(); err != nil {
				return fmt.Errorf("enable: %w", err)
			}
			fmt.Fprintf(out, "enabled %s\n", systemdUnitName)
			if !system {
				fmt.Fprintln(out, "tip: `loginctl enable-linger $USER` keeps it running across reboots without a login session")
			}
			if !now {
				scope := "--user "
				if system {
					scope = ""
				}
				fmt.Fprintf(out, "start it now with:  systemctl %s start %s\n", scope, systemdUnitName)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&system, "system", false, "install system-wide (/etc/systemd/system, needs root) instead of user-level")
	c.Flags().BoolVar(&now, "now", false, "start the service immediately after enabling")
	return c
}

func systemdUninstallCmd() *cobra.Command {
	var system bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "disable + remove the systemd unit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := unitPath(system)
			if err != nil {
				return err
			}
			if _, err := exec.LookPath("systemctl"); err == nil {
				_ = systemctl(system, "disable", "--now", systemdUnitName).Run()
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			if _, err := exec.LookPath("systemctl"); err == nil {
				_ = systemctl(system, "daemon-reload").Run()
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", path)
			return nil
		},
	}
	c.Flags().BoolVar(&system, "system", false, "operate on the system-wide unit")
	return c
}

func systemdPrintCmd() *cobra.Command {
	var system bool
	c := &cobra.Command{
		Use:   "print",
		Short: "print the systemd unit without installing it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), genUnit(exe, system))
			return nil
		},
	}
	c.Flags().BoolVar(&system, "system", false, "render the system-wide variant")
	return c
}

// systemdUnitInstalled reports whether this scope has an emx unit file, i.e.
// systemd — not emx — should own the daemon, whether or not it is running now.
func systemdUnitInstalled() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	path, err := unitPath(os.Geteuid() == 0)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// refreshSystemdUnit rewrites an installed unit whose content no longer matches
// what this build generates (a moved binary, a changed ExecStart) and reloads
// systemd, so the restart that follows runs the right thing. No unit = no-op.
func refreshSystemdUnit(out io.Writer) error {
	if !systemdUnitInstalled() {
		return nil
	}
	system := os.Geteuid() == 0
	path, _ := unitPath(system)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	want := genUnit(exe, system)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
		return nil
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		return fmt.Errorf("refresh %s: %w", path, err)
	}
	if err := systemctl(system, "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	fmt.Fprintf(out, "refreshed %s\n", path)
	return nil
}
