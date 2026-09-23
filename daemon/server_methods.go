package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
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
	e := &xray.XrayEntry{Name: req.Name, Outbound: outbound, Enabled: true, Dialer: req.Dialer, Mux: req.Mux}
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

func (s *Server) EntryDuplicate(ctx context.Context, req *emxv1.DuplicateRequest) (*emxv1.EntryReply, error) {
	dup, err := s.store.DuplicateEntry(uint(req.Id), req.NewName)
	if err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("stored, but reconcile failed: %w", err)
	}
	return &emxv1.EntryReply{Entry: entryInfo(*dup)}, nil
}

// EntryGetConfig returns the entry's raw outbound JSON, pretty-printed for editing.
func (s *Server) EntryGetConfig(ctx context.Context, req *emxv1.IdRequest) (*emxv1.ConfigReply, error) {
	e, err := s.store.GetEntry(uint(req.Id))
	if err != nil {
		return nil, err
	}
	return &emxv1.ConfigReply{Json: prettyJSON(e.Outbound)}, nil
}

// EntrySetConfig replaces the entry's outbound from edited JSON (validated as a
// JSON object) and reconciles.
func (s *Server) EntrySetConfig(ctx context.Context, req *emxv1.SetConfigRequest) (*emxv1.EntryReply, error) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(req.Json), &probe); err != nil {
		return nil, fmt.Errorf("invalid outbound JSON: %w", err)
	}
	e, err := s.store.GetEntry(uint(req.Id))
	if err != nil {
		return nil, err
	}
	e.Outbound = req.Json
	if err := s.store.UpdateEntry(e); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("saved, but reconcile failed (check your JSON): %w", err)
	}
	return &emxv1.EntryReply{Entry: entryInfo(*e)}, nil
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
			if e, err := s.store.GetEntryByName(r.Name); err == nil {
				if err := xray.CheckMemberOutbound(e.Outbound); err != nil {
					return fmt.Errorf("dialer member %q: %w", r.Name, err)
				}
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
	_, note := xray.MuxSupport(e.Outbound)
	return &emxv1.EntryInfo{
		Id: uint32(e.ID), Name: e.Name, Enabled: e.Enabled,
		IsMaster: e.IsMaster(), Dialer: e.Dialer,
		Mux: e.Mux, MuxNote: note,
	}
}

// EntrySetMux stores the entry's mux opt-in and applies it (live: the entry's
// outbound is replaced; connections through other entries are untouched).
func (s *Server) EntrySetMux(ctx context.Context, req *emxv1.SetEnabledRequest) (*emxv1.EntryReply, error) {
	e, err := s.store.GetEntry(uint(req.Id))
	if err != nil {
		return nil, err
	}
	if req.Enabled {
		if ok, why := xray.MuxSupport(e.Outbound); !ok {
			return nil, fmt.Errorf("mux can't apply to %q: %s", e.Name, why)
		}
	}
	e.Mux = req.Enabled
	if err := s.store.UpdateEntry(e); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("saved, but reconcile failed: %w", err)
	}
	return &emxv1.EntryReply{Entry: entryInfo(*e)}, nil
}

// ---- winners ---------------------------------------------------------------

func (s *Server) Winners(context.Context, *emxv1.Empty) (*emxv1.WinnersReply, error) {
	ws, err := s.sup.Winners()
	if err != nil {
		return nil, err
	}
	out := make([]*emxv1.WinnerInfo, 0, len(ws))
	for _, w := range ws {
		out = append(out, &emxv1.WinnerInfo{Master: w.Master, Node: w.Node, Nodes: w.Nodes, Tag: w.Tag, Members: int32(w.Members), Alive: int32(w.Alive)})
	}
	return &emxv1.WinnersReply{Winners: out}, nil
}

// ---- traffic ---------------------------------------------------------------

// Traffic returns per-inbound/outbound lifetime totals plus windowed totals and
// per-hour buckets (oldest→newest, ending at the current hour) for charting.
func (s *Server) Traffic(ctx context.Context, req *emxv1.TrafficRequest) (*emxv1.TrafficReply, error) {
	window := time.Duration(req.WindowSec) * time.Second
	if window <= 0 {
		window = 24 * time.Hour
	}
	if maxWindow := time.Duration(s.store.TrafficDays()) * 24 * time.Hour; window > maxWindow {
		window = maxWindow
	}
	buckets := int(window / time.Hour)
	if buckets < 1 {
		buckets = 1
	}
	nowHour := xray.HourFloor(time.Now())
	sinceHour := nowHour - int64(buckets-1)*3600

	totals, err := s.store.TrafficTotals()
	if err != nil {
		return nil, err
	}
	var inbs, outs []*emxv1.TrafficItem
	for _, t := range totals {
		bs, err := s.store.TrafficBucketsSince(t.Kind, t.Tag, sinceHour)
		if err != nil {
			return nil, err
		}
		hu := make([]int64, buckets)
		hd := make([]int64, buckets)
		var wu, wd int64
		for _, b := range bs {
			slot := int((b.HourUnix - sinceHour) / 3600)
			if slot < 0 || slot >= buckets {
				continue
			}
			hu[slot] += b.Up
			hd[slot] += b.Down
			wu += b.Up
			wd += b.Down
		}
		item := &emxv1.TrafficItem{
			Kind: t.Kind, Name: t.Name,
			TotalUp: t.Up, TotalDown: t.Down,
			WindowUp: wu, WindowDown: wd,
			HourlyUp: hu, HourlyDown: hd,
		}
		if t.Kind == xray.KindInbound {
			inbs = append(inbs, item)
		} else {
			outs = append(outs, item)
		}
	}
	sortTrafficDesc(inbs)
	sortTrafficDesc(outs)
	return &emxv1.TrafficReply{
		Inbounds: inbs, Outbounds: outs,
		WindowSec: int64(window.Seconds()), Buckets: int32(buckets),
	}, nil
}

// TrafficLive returns current cumulative counters per user tag. The CLI polls
// this and diffs consecutive samples to compute a live byte rate.
func (s *Server) TrafficLive(ctx context.Context, _ *emxv1.Empty) (*emxv1.TrafficLiveReply, error) {
	counters, err := s.sup.StatsQuery()
	if err != nil {
		return nil, err
	}
	idx := buildTagIndex(s.store)
	byKey := map[string]*emxv1.LiveItem{}
	for _, c := range counters {
		name, keep := idx.lookup(c.Kind, c.Tag)
		if !keep {
			continue
		}
		key := c.Kind + "|" + c.Tag
		li := byKey[key]
		if li == nil {
			li = &emxv1.LiveItem{Kind: c.Kind, Name: name}
			byKey[key] = li
		}
		if c.Down {
			li.Down = c.Bytes
		} else {
			li.Up = c.Bytes
		}
	}
	items := make([]*emxv1.LiveItem, 0, len(byKey))
	for _, li := range byKey {
		items = append(items, li)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	return &emxv1.TrafficLiveReply{Items: items}, nil
}

// sortTrafficDesc orders items by lifetime total (down+up) descending.
func sortTrafficDesc(items []*emxv1.TrafficItem) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].TotalDown+items[i].TotalUp > items[j].TotalDown+items[j].TotalUp
	})
}

// ---- inbounds --------------------------------------------------------------

func (s *Server) InboundAdd(ctx context.Context, req *emxv1.InboundAddRequest) (*emxv1.InboundReply, error) {
	in, err := xray.NewInboundFromTemplate(req.Name, req.Template, req.Target)
	if err != nil {
		return nil, err
	}
	in.PublicHost = req.PublicHost
	if req.Path != "" {
		in.Path = req.Path
	}
	if req.Email != "" {
		in.ClientEmail = req.Email
	}
	if req.XhttpMode != "" {
		in.XHTTPMode = req.XhttpMode
	}
	if xray.IsCaddyXHTTP(*in) && in.PublicHost == "" {
		return nil, fmt.Errorf("the vless-caddy-xhttp template requires --domain")
	}
	if err := xray.Materialize(in); err != nil {
		return nil, err
	}
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

func (s *Server) InboundDuplicate(ctx context.Context, req *emxv1.DuplicateRequest) (*emxv1.InboundReply, error) {
	dup, err := s.store.DuplicateInbound(uint(req.Id), req.NewName)
	if err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("stored, but reconcile failed: %w", err)
	}
	return &emxv1.InboundReply{Inbound: s.inboundInfo(*dup)}, nil
}

// InboundGetConfig returns the inbound as pretty JSON. All fields are editable;
// blanked required fields (uuid/keys/cert) are regenerated on save.
func (s *Server) InboundGetConfig(ctx context.Context, req *emxv1.IdRequest) (*emxv1.ConfigReply, error) {
	in, err := s.store.GetInbound(uint(req.Id))
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return nil, err
	}
	return &emxv1.ConfigReply{Json: string(b)}, nil
}

// InboundSetConfig replaces an inbound from edited JSON. The row id and creation
// time are preserved; required blanks are re-materialized; a 0 port is
// re-assigned. Then it reconciles.
func (s *Server) InboundSetConfig(ctx context.Context, req *emxv1.SetConfigRequest) (*emxv1.InboundReply, error) {
	cur, err := s.store.GetInbound(uint(req.Id))
	if err != nil {
		return nil, err
	}
	var edited xray.Inbound
	if err := json.Unmarshal([]byte(req.Json), &edited); err != nil {
		return nil, fmt.Errorf("invalid inbound JSON: %w", err)
	}
	// Preserve identity: the id and created-at can't be changed by an edit.
	edited.ID = cur.ID
	edited.CreatedAt = cur.CreatedAt
	if edited.Target, err = xray.CanonicalTarget(edited.Target); err != nil {
		return nil, err
	}
	if err := xray.Materialize(&edited); err != nil {
		return nil, err
	}
	if edited.Port == 0 && !xray.IsUnixInbound(edited) {
		// Reuse the current port if it had one, else let the store assign.
		edited.Port = cur.Port
	}
	if err := s.store.UpdateInbound(&edited); err != nil {
		return nil, err
	}
	if err := s.sup.Reconcile(); err != nil {
		return nil, fmt.Errorf("saved, but reconcile failed (check your JSON): %w", err)
	}
	return &emxv1.InboundReply{Inbound: s.inboundInfo(edited)}, nil
}

// prettyJSON re-indents a JSON string; returns it unchanged if it doesn't parse.
func prettyJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(b)
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
		TgLink:  xray.TelegramSocksLink(in, host),
		Network: in.Network, Path: in.Path, Listen: in.Listen,
		Email: in.ClientEmail, XhttpMode: in.XHTTPMode,
	}
}
