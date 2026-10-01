package daemon

import (
	"context"
	"fmt"
	"net"
	"strings"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

// Interfaces lists this server's network interfaces, for iface:NAME targets.
func (s *Server) Interfaces(context.Context, *emxv1.Empty) (*emxv1.InterfacesReply, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]*emxv1.NetInterface, 0, len(ifs))
	for _, ifc := range ifs {
		ni := &emxv1.NetInterface{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, Loopback: ifc.Flags&net.FlagLoopback != 0}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				ni.Addrs = append(ni.Addrs, a.String())
			}
		}
		out = append(out, ni)
	}
	return &emxv1.InterfacesReply{Interfaces: out}, nil
}

// checkTarget refuses a target that can't work before it is stored: an
// interface this server doesn't have, or an entry that doesn't exist (a
// master: target must name a master). Returns the canonical form.
func (s *Server) checkTarget(target string) (string, error) {
	canon, err := xray.CanonicalTarget(target)
	if err != nil {
		return "", err
	}
	kind, name, _ := xray.ParseTarget(canon)
	switch kind {
	case xray.KindIface:
		if _, err := net.InterfaceByName(name); err != nil {
			var have []string
			if ifs, err := net.Interfaces(); err == nil {
				for _, ifc := range ifs {
					have = append(have, ifc.Name)
				}
			}
			return "", fmt.Errorf("no network interface %q on this server (have: %s)", name, strings.Join(have, ", "))
		}
	case xray.KindMaster, xray.KindXray:
		e, err := s.store.GetEntryByName(name)
		if err != nil {
			return "", fmt.Errorf("no entry named %q", name)
		}
		if kind == xray.KindMaster && !e.IsMaster() {
			return "", fmt.Errorf("entry %q has no dialer — target it as xray:%s", name, name)
		}
	}
	return canon, nil
}

// InboundSetTarget points an inbound's traffic at a new egress, live.
func (s *Server) InboundSetTarget(ctx context.Context, req *emxv1.InboundTargetRequest) (*emxv1.InboundReply, error) {
	in, err := s.store.GetInbound(uint(req.Id))
	if err != nil {
		return nil, err
	}
	target, err := s.checkTarget(req.Target)
	if err != nil {
		return nil, err
	}
	in.Target = target
	if err := s.store.UpdateInbound(in); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("saved, but reconcile failed: %w", err)
	}
	return &emxv1.InboundReply{Inbound: s.inboundInfo(*in)}, nil
}
