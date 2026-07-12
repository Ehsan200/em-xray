package daemon

import (
	"context"
	"fmt"

	emxv1 "github.com/gravisun/em-xray/api/emxv1"
	"github.com/gravisun/em-xray/core/xray"
)

// ---- templates -------------------------------------------------------------

func (s *Server) TemplateList(context.Context, *emxv1.Empty) (*emxv1.TemplateListReply, error) {
	var out []*emxv1.Template
	for _, t := range xray.TemplateList() {
		out = append(out, &emxv1.Template{
			Name: t.Name, Description: t.Description,
			Protocol: t.Protocol, Network: t.Network, Security: t.Security,
		})
	}
	return &emxv1.TemplateListReply{Templates: out}, nil
}

// ---- entries ---------------------------------------------------------------

func (s *Server) EntryAdd(ctx context.Context, req *emxv1.EntryAddRequest) (*emxv1.EntryReply, error) {
	outbound := req.OutboundJson
	if req.Link != "" {
		pl, err := xray.ParseLink(req.Link)
		if err != nil {
			return nil, fmt.Errorf("parse link: %w", err)
		}
		outbound = string(pl.Outbound)
		if req.Name == "" {
			req.Name = pl.Name
		}
	}
	if outbound == "" {
		return nil, fmt.Errorf("entry needs a --link or --outbound")
	}
	if err := s.validateDialer(req.Dialer, req.Name); err != nil {
		return nil, err
	}
	e := &xray.XrayEntry{Name: req.Name, Outbound: outbound, Enabled: true, Dialer: req.Dialer}
	if err := s.store.CreateEntry(e); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("stored, but reconcile failed: %w", err)
	}
	return &emxv1.EntryReply{Entry: entryInfo(*e)}, nil
}

func (s *Server) EntryList(context.Context, *emxv1.Empty) (*emxv1.EntryListReply, error) {
	entries, err := s.store.ListEntries()
	if err != nil {
		return nil, err
	}
	var out []*emxv1.EntryInfo
	for _, e := range entries {
		out = append(out, entryInfo(e))
	}
	return &emxv1.EntryListReply{Entries: out}, nil
}

func (s *Server) EntryRemove(ctx context.Context, req *emxv1.IdRequest) (*emxv1.Empty, error) {
	if err := s.store.DeleteEntry(uint(req.Id)); err != nil {
		return nil, err
	}
	return &emxv1.Empty{}, s.sup.Reconcile()
}

func (s *Server) EntryRename(ctx context.Context, req *emxv1.RenameRequest) (*emxv1.Empty, error) {
	if err := s.store.RenameEntry(uint(req.Id), req.NewName); err != nil {
		return nil, err
	}
	return &emxv1.Empty{}, s.sup.Reconcile()
}

// validateDialer rejects a master whose dialer is malformed, references a
// missing entry/subscription, or would form a cycle. selfName is the entry being
// added/edited (treated as already stored for cycle purposes).
func (s *Server) validateDialer(dialer, selfName string) error {
	refs, err := xray.ParseDialer(dialer)
	if err != nil {
		return err
	}
	for _, r := range refs {
		switch r.Kind {
		case xray.RefXray:
			if m := s.store.NamesExist([]string{r.Name}); len(m) > 0 {
				return fmt.Errorf("dialer references unknown entry %q", r.Name)
			}
		case xray.RefXraySub:
			if m := s.store.SubNamesExist([]string{r.Name}); len(m) > 0 {
				return fmt.Errorf("dialer references unknown subscription %q", r.Name)
			}
		case xray.RefProxy:
			return fmt.Errorf("proxy dialer refs are not yet supported")
		}
	}
	return xray.DetectDialerCycle(selfName, dialer, func(name string) (string, bool) {
		e, err := s.store.GetEntryByName(name)
		if err != nil {
			return "", false
		}
		return e.Dialer, true
	})
}

func entryInfo(e xray.XrayEntry) *emxv1.EntryInfo {
	return &emxv1.EntryInfo{
		Id: uint32(e.ID), Name: e.Name, Enabled: e.Enabled,
		IsMaster: e.IsMaster(), Dialer: e.Dialer,
	}
}

// ---- winners ---------------------------------------------------------------

func (s *Server) Winners(context.Context, *emxv1.Empty) (*emxv1.WinnersReply, error) {
	ws, err := s.sup.Winners()
	if err != nil {
		return nil, err
	}
	out := make([]*emxv1.WinnerInfo, 0, len(ws))
	for _, w := range ws {
		out = append(out, &emxv1.WinnerInfo{Master: w.Master, Node: w.Node, Tag: w.Tag})
	}
	return &emxv1.WinnersReply{Winners: out}, nil
}

// ---- inbounds --------------------------------------------------------------

func (s *Server) InboundAdd(ctx context.Context, req *emxv1.InboundAddRequest) (*emxv1.InboundReply, error) {
	in, err := xray.NewInboundFromTemplate(req.Name, req.Template, req.Target)
	if err != nil {
		return nil, err
	}
	in.PublicHost = req.PublicHost
	if req.Port != 0 {
		in.Port = int(req.Port)
	}
	if err := s.store.CreateInbound(in); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("stored, but reconcile failed: %w", err)
	}
	return &emxv1.InboundReply{Inbound: s.inboundInfo(*in)}, nil
}

func (s *Server) InboundList(context.Context, *emxv1.Empty) (*emxv1.InboundListReply, error) {
	ins, err := s.store.ListInbounds()
	if err != nil {
		return nil, err
	}
	var out []*emxv1.InboundInfo
	for _, in := range ins {
		out = append(out, s.inboundInfo(in))
	}
	return &emxv1.InboundListReply{Inbounds: out}, nil
}

func (s *Server) InboundRemove(ctx context.Context, req *emxv1.IdRequest) (*emxv1.Empty, error) {
	if err := s.store.DeleteInbound(uint(req.Id)); err != nil {
		return nil, err
	}
	return &emxv1.Empty{}, s.sup.Reconcile()
}

// inboundInfo builds the wire form, resolving the share-link host: an explicit
// PublicHost wins; otherwise the daemon's auto-detected public IP is used.
func (s *Server) inboundInfo(in xray.Inbound) *emxv1.InboundInfo {
	host := in.PublicHost
	if host == "" {
		host = s.publicIP()
	}
	return &emxv1.InboundInfo{
		Id: uint32(in.ID), Name: in.Name, Protocol: in.Protocol, Port: int32(in.Port),
		Security: in.Security, Target: in.Target, Enabled: in.Enabled, Uuid: in.UUID,
		PublicHost: host, ShareLink: xray.ShareLink(in, host),
	}
}
