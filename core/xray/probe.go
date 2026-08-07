package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// Probe defaults.
const (
	DefaultProbeTimeout  = 10 * time.Second
	defaultProbeBatch    = 24 // configs per throwaway xray instance
	defaultProbeParallel = 12 // concurrent HTTP probes within a batch
	probeStartTimeout    = 8 * time.Second
)

// ProbeItem is one config to measure: the raw outbound JSON plus whatever
// identity the caller wants echoed back in the result.
type ProbeItem struct {
	ID          uint // entry id (0 for subscription nodes)
	Name        string
	Fingerprint string // subscription node fingerprint ("" for entries)
	Outbound    string // raw outbound JSON
}

// ProbeResult is one measurement. LatencyMs is meaningful only when Err is nil.
type ProbeResult struct {
	ID          uint
	Name        string
	Fingerprint string
	LatencyMs   int
	Err         error
}

// ProbeOptions carries the OS-specific bits a probe run needs.
type ProbeOptions struct {
	Bin      string        // xray executable
	AssetDir string        // XRAY_LOCATION_ASSET
	WorkDir  string        // where throwaway configs are written
	URL      string        // probe URL; "" => DefaultProbeURL
	Timeout  time.Duration // per-probe HTTP timeout; 0 => DefaultProbeTimeout
	Batch    int           // configs per xray instance; 0 => defaultProbeBatch
	Parallel int           // concurrent probes; 0 => defaultProbeParallel
}

func (o ProbeOptions) url() string {
	if o.URL == "" {
		return DefaultProbeURL
	}
	return o.URL
}

func (o ProbeOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultProbeTimeout
	}
	return o.Timeout
}

// ProbeOutbounds measures the real round-trip latency through every item: it
// starts a throwaway xray holding one loopback socks inbound per config, then
// fetches the probe URL through each. Results come back in input order, one per
// item, with Err set on failure. Items are processed in batches so a large
// subscription doesn't need hundreds of listeners at once.
//
// This is deliberately independent of the running daemon's xray — testing a
// config never touches live traffic.
func ProbeOutbounds(ctx context.Context, items []ProbeItem, opts ProbeOptions) []ProbeResult {
	out := make([]ProbeResult, len(items))
	for i, it := range items {
		out[i] = ProbeResult{ID: it.ID, Name: it.Name, Fingerprint: it.Fingerprint}
	}
	if len(items) == 0 {
		return out
	}
	if opts.Bin == "" {
		for i := range out {
			out[i].Err = fmt.Errorf("no xray binary available")
		}
		return out
	}

	// Malformed JSON can't be handed to xray at all — fail those up front so one
	// bad row doesn't sink its batch.
	idx := make([]int, 0, len(items)) // testable item indices
	for i, it := range items {
		var m map[string]any
		if err := json.Unmarshal([]byte(it.Outbound), &m); err != nil {
			out[i].Err = fmt.Errorf("bad outbound json: %w", err)
			continue
		}
		idx = append(idx, i)
	}

	batch := opts.Batch
	if batch <= 0 {
		batch = defaultProbeBatch
	}
	for start := 0; start < len(idx); start += batch {
		end := min(start+batch, len(idx))
		probeBatch(ctx, items, idx[start:end], out, opts)
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// probeBatch runs one throwaway xray for the given item indices and fills their
// results. If xray never comes up (usually one unacceptable outbound in the
// set), the batch is split in half and retried so the blame lands on the item
// that actually caused it.
func probeBatch(ctx context.Context, items []ProbeItem, idx []int, out []ProbeResult, opts ProbeOptions) {
	if len(idx) == 0 {
		return
	}
	err := runProbeBatch(ctx, items, idx, out, opts)
	if err == nil || ctx.Err() != nil {
		return
	}
	if len(idx) == 1 {
		out[idx[0]].Err = err
		return
	}
	half := len(idx) / 2
	probeBatch(ctx, items, idx[:half], out, opts)
	probeBatch(ctx, items, idx[half:], out, opts)
}

// runProbeBatch starts xray with one socks inbound per item and probes them
// concurrently. A returned error means the instance itself failed (config
// rejected, binary missing, ports never opened) — individual probe failures are
// recorded in out instead.
func runProbeBatch(ctx context.Context, items []ProbeItem, idx []int, out []ProbeResult, opts ProbeOptions) error {
	ports, err := freeLoopbackPorts(len(idx))
	if err != nil {
		return err
	}
	batch := make([]ProbeItem, len(idx))
	for i, j := range idx {
		batch[i] = items[j]
	}
	cfg, err := BuildProbeConfig(batch, ports)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(opts.WorkDir, "probe-*.json")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(cfg); err != nil {
		f.Close()
		return err
	}
	f.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, opts.Bin, "run", "-c", path)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+opts.AssetDir)
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start xray: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		cancel()
		<-exited
	}()

	if err := waitPorts(runCtx, ports, exited, probeStartTimeout); err != nil {
		if msg := stderr.String(); msg != "" {
			return fmt.Errorf("%w: %s", err, firstLine(msg))
		}
		return err
	}

	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = defaultProbeParallel
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, j := range idx {
		wg.Add(1)
		go func(port, slot int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ms, err := probeThrough(runCtx, port, opts)
			out[slot].LatencyMs, out[slot].Err = ms, err
		}(ports[i], j)
	}
	wg.Wait()
	return nil
}

// BuildProbeConfig renders the throwaway xray config for a probe batch: one
// no-auth socks inbound per item on its own loopback port, routed straight to
// that item's outbound. Pure and deterministic, so it can be unit-tested.
func BuildProbeConfig(items []ProbeItem, ports []int) ([]byte, error) {
	if len(items) != len(ports) {
		return nil, fmt.Errorf("probe config: %d items but %d ports", len(items), len(ports))
	}
	inbounds := make([]any, 0, len(items))
	// A blackhole leads the outbounds so it is xray's default handler: a probe
	// whose routing rule somehow misses must fail, not silently fall through to
	// the first item's outbound (a wrong latency attributed to the wrong node)
	// or out this box's own interface.
	outbounds := make([]any, 0, len(items)+1)
	outbounds = append(outbounds, map[string]any{"tag": "block", "protocol": "blackhole"})
	rules := make([]any, 0, len(items))
	for i, it := range items {
		inTag, outTag := fmt.Sprintf("probe-in-%d", i), fmt.Sprintf("probe-out-%d", i)
		inbounds = append(inbounds, map[string]any{
			"tag": inTag, "listen": "127.0.0.1", "port": ports[i],
			"protocol": "socks", "settings": map[string]any{"udp": false, "auth": "noauth"},
		})
		ob, err := entryOutbound(it.Outbound, outTag)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", it.Name, err)
		}
		// A master's pool wiring is a property of the live config, not of the
		// server this outbound describes — probe the server itself.
		stripDialerProxy(ob)
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{
			"type": "field", "inboundTag": []any{inTag}, "outboundTag": outTag,
		})
	}
	return json.MarshalIndent(map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"routing":   map[string]any{"rules": rules},
	}, "", "  ")
}

// stripDialerProxy removes streamSettings.sockopt.dialerProxy from an outbound
// (and the now-empty containers it leaves behind).
func stripDialerProxy(outbound map[string]any) {
	ss, ok := outbound["streamSettings"].(map[string]any)
	if !ok {
		return
	}
	sockopt, ok := ss["sockopt"].(map[string]any)
	if !ok {
		return
	}
	delete(sockopt, "dialerProxy")
	if len(sockopt) == 0 {
		delete(ss, "sockopt")
	}
}

// probeThrough fetches the probe URL through the socks proxy on port and returns
// the round-trip in milliseconds.
func probeThrough(ctx context.Context, port int, opts ProbeOptions) (int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		return 0, err
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return 0, fmt.Errorf("socks dialer lacks context support")
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         ctxDialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: opts.timeout(),
		},
		// Redirects would measure a second hop; the probe URL answers 204.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	reqCtx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opts.url(), nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, probeErr(err)
	}
	defer resp.Body.Close()
	ms := int(time.Since(start).Milliseconds())
	if ms == 0 {
		ms = 1 // a sub-millisecond round trip still means "reachable"
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	return ms, nil
}

// probeErr trims the noisy URL/proxy wrapping off a failed probe.
func probeErr(err error) error {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 && strings.Contains(msg, "socks connect") {
		msg = msg[i+2:]
	}
	if strings.Contains(msg, "context deadline exceeded") {
		return fmt.Errorf("timeout")
	}
	return fmt.Errorf("%s", msg)
}

// freeLoopbackPorts reserves n distinct free loopback ports by binding them all
// at once (so the kernel can't hand out the same one twice) and releasing them.
func freeLoopbackPorts(n int) ([]int, error) {
	lns := make([]net.Listener, 0, n)
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve probe port: %w", err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// waitPorts blocks until every port accepts a connection, xray exits, or the
// timeout elapses.
func waitPorts(ctx context.Context, ports []int, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, p := range ports {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-exited:
				return fmt.Errorf("xray exited before it was ready")
			default:
			}
			conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("xray did not start within %s", timeout)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil
}

// tailBuffer keeps only the last few KB written to it — enough for the xray
// startup error, without letting a chatty child grow unbounded.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferMax = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferMax {
		t.buf = t.buf[len(t.buf)-tailBufferMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// firstLine returns the first non-empty line of s, for compact error messages.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}
