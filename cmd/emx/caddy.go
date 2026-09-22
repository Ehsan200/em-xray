package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

const caddyConfigPath = "/etc/caddy/Caddyfile"

var (
	cloudflareHTTPPorts  = []int{80, 8080, 8880, 2052, 2082, 2086, 2095}
	cloudflareHTTPSPorts = []int{443, 2053, 2083, 2087, 2096, 8443}
)

func caddyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "caddy",
		Short: "install and manage Caddy for XHTTP inbounds",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMenu(cmd, "caddy") },
	}
	c.AddCommand(caddyInstallCmd(), caddyPrintCmd(), caddyApplyCmd(),
		caddyDomainsCmd(), caddyEnableCmd(), caddyDisableCmd(), caddyStatusCmd())
	return c
}

func caddyInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "install the official Caddy package (Debian/Ubuntu)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("Caddy installation is Linux-only (this is %s)", runtime.GOOS)
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("Caddy installation needs root; run `sudo emx caddy install`")
			}
			if path, err := exec.LookPath("caddy"); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Caddy is already installed: %s\n", path)
				return nil
			}
			if _, err := exec.LookPath("apt-get"); err != nil {
				return fmt.Errorf("automatic installation currently supports Debian/Ubuntu (apt-get not found)")
			}
			keyFile, err := os.CreateTemp("", "emx-caddy-key-*.gpg")
			if err != nil {
				return err
			}
			keyPath := keyFile.Name()
			if err := keyFile.Close(); err != nil {
				return err
			}
			defer os.Remove(keyPath)
			steps := [][]string{
				{"apt-get", "update"},
				{"apt-get", "install", "-y", "debian-keyring", "debian-archive-keyring", "apt-transport-https", "curl", "gnupg"},
				{"curl", "-1sLf", "https://dl.cloudsmith.io/public/caddy/stable/gpg.key", "-o", keyPath},
				{"gpg", "--dearmor", "--yes", "--output", "/usr/share/keyrings/caddy-stable-archive-keyring.gpg", keyPath},
				{"curl", "-1sLf", "https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt", "-o", "/etc/apt/sources.list.d/caddy-stable.list"},
				{"chmod", "o+r", "/usr/share/keyrings/caddy-stable-archive-keyring.gpg", "/etc/apt/sources.list.d/caddy-stable.list"},
				{"apt-get", "update"},
				{"apt-get", "install", "-y", "caddy"},
			}
			for _, step := range steps {
				fmt.Fprintf(cmd.OutOrStdout(), "running: %s\n", strings.Join(step, " "))
				c := exec.CommandContext(cmd.Context(), step[0], step[1:]...)
				c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
				if err := c.Run(); err != nil {
					return fmt.Errorf("%s: %w", step[0], err)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Caddy installed and started")
			return nil
		},
	}
}

func caddyPrintCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "print",
		Aliases: []string{"config"},
		Short:   "print the Caddyfile generated from Caddy/XHTTP inbounds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadCaddyConfig(cmd)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), cfg)
			return nil
		},
	}
}

func caddyDomainsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "domains",
		Aliases: []string{"ls", "list"},
		Short:   "list domains managed by Caddy/XHTTP inbounds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, inbounds, err := loadCaddyConfig(cmd)
			if err != nil {
				return err
			}
			if len(inbounds) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no Caddy/XHTTP inbounds")
				return nil
			}
			for _, in := range inbounds {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", in.PublicHost, in.Path, in.Name)
			}
			return nil
		},
	}
}

func caddyApplyCmd() *cobra.Command {
	var path string
	var noReload bool
	c := &cobra.Command{
		Use:   "apply",
		Short: "validate, install, and reload the generated Caddyfile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == caddyConfigPath && os.Geteuid() != 0 {
				return fmt.Errorf("writing %s needs root; run `sudo emx caddy apply`", path)
			}
			if _, err := exec.LookPath("caddy"); err != nil {
				return fmt.Errorf("caddy is not installed; run `sudo emx caddy install`")
			}
			cfg, inbounds, err := loadCaddyConfig(cmd)
			if err != nil {
				return err
			}
			if len(inbounds) == 0 {
				stop := exec.CommandContext(cmd.Context(), "systemctl", "disable", "--now", "caddy")
				if out, err := stop.CombinedOutput(); err != nil {
					return fmt.Errorf("no Caddy/XHTTP inbounds remain, but Caddy could not be disabled: %v: %s", err, strings.TrimSpace(string(out)))
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no Caddy/XHTTP inbounds remain; Caddy disabled")
				return nil
			}
			if err := applyCaddyConfig(cmd, path, []byte(cfg), noReload); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied %d Caddy/XHTTP inbound(s) to %s\n", len(inbounds), path)
			return nil
		},
	}
	c.Flags().StringVar(&path, "config", caddyConfigPath, "Caddyfile to manage")
	c.Flags().BoolVar(&noReload, "no-reload", false, "write the validated config without reloading Caddy")
	return c
}

func caddyEnableCmd() *cobra.Command {
	return caddyServiceCmd("enable", "enable and start Caddy", "enable", "--now", "caddy")
}

func caddyDisableCmd() *cobra.Command {
	return caddyServiceCmd("disable", "stop and disable Caddy", "disable", "--now", "caddy")
}

func caddyServiceCmd(use, short string, args ...string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("Caddy service management is Linux-only")
			}
			if os.Geteuid() != 0 {
				return fmt.Errorf("this command needs root; run `sudo emx caddy %s`", use)
			}
			c := exec.CommandContext(cmd.Context(), "systemctl", args...)
			c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := c.Run(); err != nil {
				return fmt.Errorf("systemctl %s: %w", use, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Caddy %sd\n", use)
			return nil
		},
	}
}

func caddyStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "show Caddy installation and service status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := exec.LookPath("caddy")
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "caddy: not installed")
				return nil
			}
			version, _ := exec.CommandContext(cmd.Context(), path, "version").Output()
			fmt.Fprintf(cmd.OutOrStdout(), "caddy: installed (%s)\n", strings.TrimSpace(string(version)))
			if runtime.GOOS == "linux" {
				active := exec.CommandContext(cmd.Context(), "systemctl", "is-active", "--quiet", "caddy").Run() == nil
				enabled := exec.CommandContext(cmd.Context(), "systemctl", "is-enabled", "--quiet", "caddy").Run() == nil
				fmt.Fprintf(cmd.OutOrStdout(), "service: active=%v enabled=%v\n", active, enabled)
			}
			return nil
		},
	}
}

func loadCaddyConfig(cmd *cobra.Command) (string, []*emxv1.InboundInfo, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	client, conn, err := dialReady(ctx)
	if err != nil {
		return "", nil, errDaemon(err)
	}
	defer conn.Close()
	reply, err := client.InboundList(ctx, &emxv1.Empty{})
	if err != nil {
		return "", nil, err
	}
	return renderCaddyfile(reply.Inbounds)
}

func renderCaddyfile(all []*emxv1.InboundInfo) (string, []*emxv1.InboundInfo, error) {
	var managed []*emxv1.InboundInfo
	byDomain := map[string][]*emxv1.InboundInfo{}
	routes := map[string]string{}
	for _, in := range all {
		if !in.Enabled || in.Network != "xhttp" || in.Security != "none" || in.Port != 0 || in.Listen == "" {
			continue
		}
		if err := validateCaddyDomain(in.PublicHost); err != nil {
			return "", nil, fmt.Errorf("inbound %q: %w", in.Name, err)
		}
		if err := validateCaddyPath(in.Path); err != nil {
			return "", nil, fmt.Errorf("inbound %q: %w", in.Name, err)
		}
		if err := validateCaddySocket(in.Listen); err != nil {
			return "", nil, fmt.Errorf("inbound %q: %w", in.Name, err)
		}
		routeKey := in.PublicHost + "\x00" + in.Path
		if other := routes[routeKey]; other != "" {
			return "", nil, fmt.Errorf("inbounds %q and %q use the same domain and path", other, in.Name)
		}
		routes[routeKey] = in.Name
		managed = append(managed, in)
		byDomain[in.PublicHost] = append(byDomain[in.PublicHost], in)
	}
	sort.Slice(managed, func(i, j int) bool {
		if managed[i].PublicHost != managed[j].PublicHost {
			return managed[i].PublicHost < managed[j].PublicHost
		}
		return managed[i].Path < managed[j].Path
	})
	domains := make([]string, 0, len(byDomain))
	for domain := range byDomain {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	var b strings.Builder
	b.WriteString("# Generated by emx. Changes are replaced by `emx caddy apply`.\n\n")
	for _, domain := range domains {
		fmt.Fprintf(&b, "%s {\n", caddySiteAddresses(domain))
		ins := byDomain[domain]
		sort.Slice(ins, func(i, j int) bool { return ins[i].Path < ins[j].Path })
		for i, in := range ins {
			matcher := fmt.Sprintf("emx_%d", i+1)
			fmt.Fprintf(&b, "\t@%s path %s*\n", matcher, in.Path)
			fmt.Fprintf(&b, "\treverse_proxy @%s unix/%s\n", matcher, socketPath(in.Listen))
		}
		b.WriteString("}\n\n")
	}
	return b.String(), managed, nil
}

func caddySiteAddresses(domain string) string {
	addresses := make([]string, 0, len(cloudflareHTTPPorts)+len(cloudflareHTTPSPorts))
	for _, port := range cloudflareHTTPPorts {
		addresses = append(addresses, fmt.Sprintf("http://%s:%d", domain, port))
	}
	for _, port := range cloudflareHTTPSPorts {
		addresses = append(addresses, fmt.Sprintf("https://%s:%d", domain, port))
	}
	return strings.Join(addresses, ", ")
}

func validateCaddyDomain(domain string) error {
	if domain == "" {
		return fmt.Errorf("domain is empty")
	}
	if net.ParseIP(domain) != nil {
		return fmt.Errorf("domain %q is an IP address", domain)
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("invalid domain %q", domain)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' {
				return fmt.Errorf("invalid domain %q", domain)
			}
		}
	}
	return nil
}

func validateCaddyPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("path %q must start with /", path)
	}
	if strings.ContainsAny(path, " \t\r\n{}#") {
		return fmt.Errorf("path %q contains characters unsafe in a Caddyfile", path)
	}
	return nil
}

func validateCaddySocket(listen string) error {
	path := socketPath(listen)
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("socket path %q must be absolute", path)
	}
	if strings.ContainsAny(path, " \t\r\n{}#") {
		return fmt.Errorf("socket path %q contains characters unsafe in a Caddyfile", path)
	}
	return nil
}

func socketPath(listen string) string {
	path, _, _ := strings.Cut(listen, ",")
	return path
}

func applyCaddyConfig(cmd *cobra.Command, path string, cfg []byte, noReload bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".emx-tmp"
	if err := os.WriteFile(tmp, cfg, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	validate := exec.CommandContext(cmd.Context(), "caddy", "validate", "--config", tmp, "--adapter", "caddyfile")
	if out, err := validate.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy validation failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	old, readErr := os.ReadFile(path)
	unchanged := readErr == nil && string(old) == string(cfg)
	if unchanged {
		fmt.Fprintln(cmd.OutOrStdout(), "Caddy configuration is already current")
	}
	backup := ""
	if !unchanged && readErr == nil {
		backup = path + ".emx-backup-" + time.Now().Format("20060102-150405.000000000")
		if err := os.WriteFile(backup, old, 0o644); err != nil {
			return fmt.Errorf("backup existing Caddyfile: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "backed up existing config to %s\n", backup)
	}
	if !unchanged {
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	if noReload {
		return nil
	}
	rollback := func() {
		if backup != "" {
			_ = os.WriteFile(path, old, 0o644)
		}
	}
	if exec.CommandContext(cmd.Context(), "systemctl", "is-active", "--quiet", "caddy").Run() != nil {
		start := exec.CommandContext(cmd.Context(), "systemctl", "enable", "--now", "caddy")
		if out, err := start.CombinedOutput(); err != nil {
			rollback()
			return fmt.Errorf("start caddy: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	reload := exec.CommandContext(cmd.Context(), "caddy", "reload", "--config", path, "--adapter", "caddyfile")
	if out, err := reload.CombinedOutput(); err != nil {
		rollback()
		return fmt.Errorf("reload caddy: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// maybeApplyCaddy keeps Caddy in step after a managed inbound changes. It is a
// no-op before Caddy is installed; non-root callers get a precise follow-up
// command because the normal system Caddyfile is root-owned.
func maybeApplyCaddy(cmd *cobra.Command) error {
	if _, err := exec.LookPath("caddy"); err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "Caddy is not installed yet; run `sudo emx caddy install`, then `sudo emx caddy apply`")
		return nil
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Caddy needs applying: run `sudo emx caddy apply`")
		return nil
	}
	cfg, inbounds, err := loadCaddyConfig(cmd)
	if err != nil {
		return err
	}
	if len(inbounds) == 0 {
		stop := exec.CommandContext(cmd.Context(), "systemctl", "disable", "--now", "caddy")
		if out, err := stop.CombinedOutput(); err != nil {
			return fmt.Errorf("disable Caddy after removing its last inbound: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintln(cmd.OutOrStdout(), "no Caddy/XHTTP inbounds remain; Caddy disabled")
		return nil
	}
	return applyCaddyConfig(cmd, caddyConfigPath, []byte(cfg), false)
}

func isCaddyInboundInfo(in *emxv1.InboundInfo) bool {
	return in != nil && in.Network == "xhttp" && in.Security == "none" && in.Port == 0 && in.Listen != ""
}

func isCaddyInboundJSON(raw string) bool {
	var in struct {
		Network  string
		Security string
		Listen   string
		Port     int
	}
	if json.Unmarshal([]byte(raw), &in) != nil {
		return false
	}
	return in.Network == "xhttp" && in.Security == "none" && in.Port == 0 && in.Listen != ""
}
