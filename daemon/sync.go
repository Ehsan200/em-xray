package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gravisun/em-xray/core/xray"
)

// SyncDialerMembers is the live, no-restart update path for routine node churn
// (a subscription refresh, a node enable/disable, a cap change). It diffs the
// desired pool against what xray has live and applies the delta over the gRPC
// api. It falls back to a full Reconcile only when the *set* of slotted masters
// changed (a structural change that needs new baked inbounds/balancers).
//
// Wired to subFetcher.onChange, so a successful refresh never restarts xray.
func (s *Supervisor) SyncDialerMembers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.syncLocked(); err != nil {
		s.log.Printf("sync: %v", err)
	}
}

func (s *Supervisor) syncLocked() error {
	if !s.wd.IsStarted() {
		return nil // xray not running; the next Reconcile bakes the pool at start
	}
	entries, err := s.store.ListEntries()
	if err != nil {
		return err
	}
	desired, err := s.resolveDialerSlots(entries)
	if err != nil {
		return err
	}
	if !sameSlotMasters(desired, s.loadedSlots) {
		s.log.Print("sync: slot master-set changed → full reconcile")
		return s.reconcileLocked()
	}
	return s.applyMemberDeltaLocked(desired)
}

// applyMemberDeltaLocked converges each slot's live members to desired via
// rmo/ado. loadedSlots is advanced PER SLOT after that slot succeeds, so a
// mid-way failure still records the slots that converged; the next sync retries
// only the rest. Callers must hold s.mu.
func (s *Supervisor) applyMemberDeltaLocked(desired []xray.Slot) error {
	for _, sl := range desired {
		ls := s.loadedSlots[sl.Master]
		adds, removeKeys := computeDelta(ls.keys, sl.Members)
		if len(adds) == 0 && len(removeKeys) == 0 {
			continue
		}

		// Removals first. "Already gone" is tolerated — log and keep going.
		var rmTags []string
		for _, k := range removeKeys {
			rmTags = append(rmTags, xray.SlotMemberTag(ls.idx, k))
		}
		if err := s.apiRemoveOutbounds(rmTags...); err != nil {
			s.log.Printf("sync %q: rmo (tolerated): %v", sl.Master, err)
		}

		// Additions: write one-outbound files, then ado them.
		files, err := s.writeMemberFiles(ls.idx, adds)
		if err != nil {
			s.log.Printf("sync %q: write member files: %v", sl.Master, err)
			continue // don't advance; retry next sync
		}
		if err := s.apiAddOutbounds(files...); err != nil {
			s.log.Printf("sync %q: ado: %v", sl.Master, err)
			continue
		}

		// Slot converged — advance its live key set.
		newKeys := make(map[string]bool, len(sl.Members))
		for _, m := range sl.Members {
			newKeys[m.Key] = true
		}
		ls.keys = newKeys
		s.loadedSlots[sl.Master] = ls
		s.log.Printf("sync %q: +%d -%d members (no restart)", sl.Master, len(adds), len(removeKeys))
	}
	return nil
}

// writeMemberFiles writes each member as a {"outbounds":[...]} file (the form
// `xray api ado` consumes) and returns the paths.
func (s *Supervisor) writeMemberFiles(idx int, members []xray.SlotMember) ([]string, error) {
	dir := s.paths.AdoDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var files []string
	for _, m := range members {
		ob, err := xray.MemberOutboundJSON(idx, m)
		if err != nil {
			return nil, err
		}
		wrapped := map[string]any{"outbounds": []any{ob}}
		data, err := json.MarshalIndent(wrapped, "", "  ")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, xray.SlotMemberTag(idx, m.Key)+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		files = append(files, path)
	}
	return files, nil
}

// computeDelta returns members to add (not currently live) and keys to remove
// (live but no longer desired). Because keys are content fingerprints, a changed
// node is a clean remove+add, never a same-key mutation.
func computeDelta(loadedKeys map[string]bool, desired []xray.SlotMember) (adds []xray.SlotMember, removeKeys []string) {
	desiredSet := make(map[string]bool, len(desired))
	for _, m := range desired {
		desiredSet[m.Key] = true
		if !loadedKeys[m.Key] {
			adds = append(adds, m)
		}
	}
	for k := range loadedKeys {
		if !desiredSet[k] {
			removeKeys = append(removeKeys, k)
		}
	}
	return adds, removeKeys
}

// sameSlotMasters reports whether the desired slots and the live-loaded slots
// cover the exact same set of masters.
func sameSlotMasters(desired []xray.Slot, loaded map[string]loadedSlot) bool {
	if len(desired) != len(loaded) {
		return false
	}
	for _, sl := range desired {
		if _, ok := loaded[sl.Master]; !ok {
			return false
		}
	}
	return true
}
