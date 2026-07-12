package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/ehsan200/em-xray/core/xray"
)

// backupVersion is bumped if the backup shape changes incompatibly.
const backupVersion = 1

// configBackup is the portable snapshot of a box's config. Volatile subscription
// nodes are intentionally excluded — they're re-fetched after import.
type configBackup struct {
	Version       int                 `json:"version"`
	ExportedAt    string              `json:"exported_at"`
	Inbounds      []xray.Inbound      `json:"inbounds"`
	Entries       []xray.XrayEntry    `json:"entries"`
	Subscriptions []xray.Subscription `json:"subscriptions"`
}

// ExportConfig returns the full config as pretty JSON for backup.
func (s *Server) ExportConfig(ctx context.Context, _ *emxv1.Empty) (*emxv1.ConfigReply, error) {
	ins, err := s.store.ListInbounds()
	if err != nil {
		return nil, err
	}
	ents, err := s.store.ListEntries()
	if err != nil {
		return nil, err
	}
	subs, err := s.store.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	b := configBackup{
		Version: backupVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Inbounds: ins, Entries: ents, Subscriptions: subs,
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return &emxv1.ConfigReply{Json: string(raw)}, nil
}

// ImportConfig restores a backup. replace=true wipes existing config first;
// otherwise it merges, skipping any name that already exists. IDs and timestamps
// from the backup are dropped so rows are re-created cleanly.
func (s *Server) ImportConfig(ctx context.Context, req *emxv1.ImportRequest) (*emxv1.ImportReply, error) {
	var b configBackup
	if err := json.Unmarshal([]byte(req.Json), &b); err != nil {
		return nil, fmt.Errorf("invalid backup JSON: %w", err)
	}
	if b.Version > backupVersion {
		return nil, fmt.Errorf("backup version %d newer than supported %d", b.Version, backupVersion)
	}
	if req.Replace {
		if err := s.store.DeleteAllConfig(); err != nil {
			return nil, fmt.Errorf("wipe existing: %w", err)
		}
	}

	reply := &emxv1.ImportReply{}
	for _, in := range b.Inbounds {
		if !req.Replace {
			if _, err := s.store.GetInboundByName(in.Name); err == nil {
				reply.Skipped = append(reply.Skipped, "inbound:"+in.Name)
				continue
			}
		}
		in.ID, in.CreatedAt, in.UpdatedAt = 0, time.Time{}, time.Time{}
		if err := s.store.CreateInbound(&in); err != nil {
			return nil, fmt.Errorf("import inbound %q: %w", in.Name, err)
		}
		reply.Inbounds++
	}
	for _, e := range b.Entries {
		if !req.Replace {
			if _, err := s.store.GetEntryByName(e.Name); err == nil {
				reply.Skipped = append(reply.Skipped, "entry:"+e.Name)
				continue
			}
		}
		e.ID, e.CreatedAt, e.UpdatedAt = 0, time.Time{}, time.Time{}
		if err := s.store.CreateEntry(&e); err != nil {
			return nil, fmt.Errorf("import entry %q: %w", e.Name, err)
		}
		reply.Entries++
	}
	for _, sub := range b.Subscriptions {
		if !req.Replace {
			if _, err := s.store.GetSubscriptionByName(sub.Name); err == nil {
				reply.Skipped = append(reply.Skipped, "sub:"+sub.Name)
				continue
			}
		}
		sub.ID, sub.CreatedAt, sub.UpdatedAt = 0, time.Time{}, time.Time{}
		if err := s.store.CreateSubscription(&sub); err != nil {
			return nil, fmt.Errorf("import subscription %q: %w", sub.Name, err)
		}
		reply.Subs++
	}

	if err := s.sup.Reconcile(); err != nil {
		return reply, fmt.Errorf("imported, but reconcile failed: %w", err)
	}
	// Fetch nodes for freshly-imported subscriptions in the background.
	go s.fetcher.RefreshAll(context.Background())
	return reply, nil
}
