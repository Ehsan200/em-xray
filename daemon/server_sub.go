package daemon

import (
	"context"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/gravisun/em-xray/core/xray"
)

func (s *Server) SubAdd(ctx context.Context, req *emxv1.SubAddRequest) (*emxv1.SubReply, error) {
	sub := &xray.Subscription{
		Name: req.Name, URL: req.Url, UserAgent: req.UserAgent,
		IntervalSec: int(req.IntervalSec), NodeCap: int(req.NodeCap), Enabled: true,
	}
	if err := s.store.CreateSubscription(sub); err != nil {
		return nil, err
	}
	// Populate immediately so the pool is usable at once (fires SyncDialerMembers).
	_, _ = s.fetcher.RefreshOne(ctx, sub.ID)
	fresh, err := s.store.GetSubscription(sub.ID)
	if err != nil {
		return nil, err
	}
	return &emxv1.SubReply{Sub: s.subInfo(*fresh)}, nil
}

func (s *Server) SubList(context.Context, *emxv1.Empty) (*emxv1.SubListReply, error) {
	subs, err := s.store.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	var out []*emxv1.SubInfo
	for _, sub := range subs {
		out = append(out, s.subInfo(sub))
	}
	return &emxv1.SubListReply{Subs: out}, nil
}

func (s *Server) SubRemove(ctx context.Context, req *emxv1.IdRequest) (*emxv1.Empty, error) {
	if err := s.store.DeleteSubscription(uint(req.Id)); err != nil {
		return nil, err
	}
	s.sup.SyncDialerMembers() // pool shrinks live (or reconciles if a slot vanished)
	return &emxv1.Empty{}, nil
}

func (s *Server) SubSetEnabled(ctx context.Context, req *emxv1.SetEnabledRequest) (*emxv1.Empty, error) {
	if err := s.store.SetSubEnabled(uint(req.Id), req.Enabled); err != nil {
		return nil, err
	}
	s.sup.SyncDialerMembers()
	return &emxv1.Empty{}, nil
}

func (s *Server) SubRefresh(ctx context.Context, req *emxv1.SubRefreshRequest) (*emxv1.SubRefreshReply, error) {
	if req.Id == 0 {
		return &emxv1.SubRefreshReply{}, s.fetcher.RefreshAll(ctx)
	}
	n, err := s.fetcher.RefreshOne(ctx, uint(req.Id))
	if err != nil {
		return nil, err
	}
	return &emxv1.SubRefreshReply{Nodes: int32(n)}, nil
}

func (s *Server) SubNodes(ctx context.Context, req *emxv1.IdRequest) (*emxv1.SubNodesReply, error) {
	nodes, err := s.store.NodesForSub(uint(req.Id))
	if err != nil {
		return nil, err
	}
	disabled, err := s.store.DisabledFingerprints(uint(req.Id))
	if err != nil {
		return nil, err
	}
	var out []*emxv1.NodeInfo
	for _, n := range nodes {
		out = append(out, &emxv1.NodeInfo{
			Fingerprint: n.Fingerprint, Name: n.Name, Active: n.Active,
			Disabled: disabled[n.Fingerprint], LatencyMs: int32(n.LastLatencyMs),
		})
	}
	return &emxv1.SubNodesReply{Nodes: out}, nil
}

func (s *Server) SubSetNodeDisabled(ctx context.Context, req *emxv1.SubNodeDisabledRequest) (*emxv1.Empty, error) {
	if err := s.store.SetNodeDisabled(uint(req.SubId), req.Fingerprint, req.Disabled); err != nil {
		return nil, err
	}
	s.sup.SyncDialerMembers() // the (de)activated node enters/leaves the pool live
	return &emxv1.Empty{}, nil
}

func (s *Server) SubRename(ctx context.Context, req *emxv1.RenameRequest) (*emxv1.Empty, error) {
	if err := s.store.RenameSubscription(uint(req.Id), req.NewName); err != nil {
		return nil, err
	}
	s.sup.SyncDialerMembers() // dialer refs rewritten → pool unchanged, but reconcile if needed
	return &emxv1.Empty{}, nil
}

func (s *Server) subInfo(sub xray.Subscription) *emxv1.SubInfo {
	nodes, _ := s.store.NodesForSub(sub.ID)
	active, _ := s.store.ActiveNodes(sub.ID)
	return &emxv1.SubInfo{
		Id: uint32(sub.ID), Name: sub.Name, Url: sub.URL, Enabled: sub.Enabled,
		NodeCount: int32(len(nodes)), ActiveCount: int32(len(active)), LastError: sub.LastError,
		Upload: sub.Upload, Download: sub.Download, Total: sub.Total, Expire: sub.Expire,
	}
}
