package daemon

import (
	"context"
	"fmt"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

func (s *Server) InboundUserAdd(ctx context.Context, req *emxv1.InboundUserAddRequest) (*emxv1.InboundUserReply, error) {
	in, err := s.store.GetInbound(uint(req.InboundId))
	if err != nil {
		return nil, err
	}
	u, err := xray.NewInboundUser(in, req.Name, req.ByteCap)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateInboundUser(u); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("stored, but reconcile failed: %w", err)
	}
	return &emxv1.InboundUserReply{User: s.userInfo(*in, *u)}, nil
}

func (s *Server) InboundUserList(ctx context.Context, req *emxv1.IdRequest) (*emxv1.InboundUserListReply, error) {
	in, err := s.store.GetInbound(uint(req.Id))
	if err != nil {
		return nil, err
	}
	users, err := s.store.InboundUsers(uint(req.Id))
	if err != nil {
		return nil, err
	}
	out := make([]*emxv1.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, s.userInfo(*in, u))
	}
	return &emxv1.InboundUserListReply{Users: out}, nil
}

func (s *Server) InboundUserRemove(ctx context.Context, req *emxv1.IdRequest) (*emxv1.Empty, error) {
	if err := s.store.DeleteInboundUser(uint(req.Id)); err != nil {
		return nil, err
	}
	return &emxv1.Empty{}, s.sup.Reconcile()
}

func (s *Server) InboundUserSetEnabled(ctx context.Context, req *emxv1.SetEnabledRequest) (*emxv1.Empty, error) {
	if err := s.store.SetInboundUserEnabled(uint(req.Id), req.Enabled); err != nil {
		return nil, err
	}
	return &emxv1.Empty{}, s.sup.Reconcile()
}

// userInfo builds the wire form for an inbound user, resolving its per-user
// usage, over-cap state and its own share link.
func (s *Server) userInfo(in xray.Inbound, u xray.InboundUser) *emxv1.UserInfo {
	host := in.PublicHost
	if host == "" {
		host = s.publicIP()
	}
	up, down := s.store.TrafficTotalFor(xray.KindUser, u.Email)
	over := u.ByteCap > 0 && up+down >= u.ByteCap
	return &emxv1.UserInfo{
		Id: uint32(u.ID), Name: u.Name, Enabled: u.Enabled, ByteCap: u.ByteCap,
		UsedUp: up, UsedDown: down, OverCap: over,
		ShareLink: xray.ShareLinkForUser(in, host, u),
	}
}
