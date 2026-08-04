package daemon

import (
	"context"
	"fmt"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// Test kinds accepted by the Test RPC.
const (
	TestKindEntry   = "entry"   // one entry, by id
	TestKindEntries = "entries" // every enabled entry
	TestKindSub     = "sub"     // every node of a subscription
	TestKindNode    = "node"    // one node of a subscription, by fingerprint
)

// XrayRestart force-cycles the xray child (regenerating config.json first). Use
// it when xray is wedged — a stuck balancer, listeners that stopped answering —
// without bouncing the daemon and losing the control socket.
func (s *Server) XrayRestart(ctx context.Context, _ *emxv1.Empty) (*emxv1.XrayRestartReply, error) {
	running, pid, err := s.sup.RestartXray()
	if err != nil {
		return nil, err
	}
	msg := "xray restarted"
	if !running {
		msg = "nothing routable — xray is stopped (add or enable an inbound)"
	}
	return &emxv1.XrayRestartReply{Running: running, Pid: int32(pid), Message: msg}, nil
}

// Test measures the real round-trip latency through one or more stored configs:
// a throwaway xray is started with a loopback socks inbound per config and the
// probe URL is fetched through each. It never touches the live xray. Latencies
// for subscription nodes are persisted so the node views can show them.
func (s *Server) Test(ctx context.Context, req *emxv1.TestRequest) (*emxv1.TestReply, error) {
	items, subID, err := s.testItems(req)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return &emxv1.TestReply{}, nil
	}
	bin, err := xraybin.Extract(s.sup.paths.Cache)
	if err != nil {
		return nil, err
	}
	opts := xray.ProbeOptions{
		Bin:      bin,
		AssetDir: s.sup.paths.AssetDir(),
		WorkDir:  s.sup.paths.Runtime,
		URL:      req.Url,
	}
	if req.TimeoutSec > 0 {
		opts.Timeout = time.Duration(req.TimeoutSec) * time.Second
	}

	results := xray.ProbeOutbounds(ctx, items, opts)
	out := make([]*emxv1.TestResult, 0, len(results))
	for _, r := range results {
		tr := &emxv1.TestResult{
			Id: uint32(r.ID), Name: r.Name, Fingerprint: r.Fingerprint, LatencyMs: int32(r.LatencyMs),
		}
		if r.Err != nil {
			tr.Error = r.Err.Error()
		}
		if subID != 0 && r.Fingerprint != "" {
			// A failed probe clears the stale figure rather than keeping a lie.
			_ = s.store.SetNodeLatency(subID, r.Fingerprint, r.LatencyMs)
		}
		out = append(out, tr)
	}
	return &emxv1.TestReply{Results: out}, nil
}

// testItems resolves a TestRequest into the configs to probe. subID is non-zero
// when the items are subscription nodes (so their latency can be persisted).
func (s *Server) testItems(req *emxv1.TestRequest) ([]xray.ProbeItem, uint, error) {
	switch req.Kind {
	case TestKindEntry:
		e, err := s.store.GetEntry(uint(req.Id))
		if err != nil {
			return nil, 0, err
		}
		return []xray.ProbeItem{{ID: e.ID, Name: e.Name, Outbound: e.Outbound}}, 0, nil

	case TestKindEntries, "":
		entries, err := s.store.ListEntries()
		if err != nil {
			return nil, 0, err
		}
		var items []xray.ProbeItem
		for _, e := range entries {
			items = append(items, xray.ProbeItem{ID: e.ID, Name: e.Name, Outbound: e.Outbound})
		}
		return items, 0, nil

	case TestKindSub:
		sub, err := s.store.GetSubscription(uint(req.Id))
		if err != nil {
			return nil, 0, err
		}
		nodes, err := s.store.NodesForSub(sub.ID)
		if err != nil {
			return nil, 0, err
		}
		var items []xray.ProbeItem
		for _, n := range nodes {
			items = append(items, xray.ProbeItem{Name: n.Name, Fingerprint: n.Fingerprint, Outbound: n.Outbound})
		}
		return items, sub.ID, nil

	case TestKindNode:
		nodes, err := s.store.NodesForSub(uint(req.Id))
		if err != nil {
			return nil, 0, err
		}
		for _, n := range nodes {
			if n.Fingerprint == req.Fingerprint {
				return []xray.ProbeItem{{Name: n.Name, Fingerprint: n.Fingerprint, Outbound: n.Outbound}}, uint(req.Id), nil
			}
		}
		return nil, 0, fmt.Errorf("no node %q in subscription %d", req.Fingerprint, req.Id)

	default:
		return nil, 0, fmt.Errorf("unknown test kind %q (want entry|entries|sub|node)", req.Kind)
	}
}
