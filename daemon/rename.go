package daemon

import "github.com/ehsan200/em-xray/core/xray"

// RenamedRef carries the daemon's in-memory pool state across an entry
// (xray.RefXray) or subscription (xray.RefXraySub) rename, which the store
// has already cascaded into every dialer. Pools are keyed by their refs and
// entry members by name, so without this a rename would look like a new pool
// — possibly renumbered, its ranking and parked nodes forgotten, and every
// master on it back on leastLoad until the next poll. Call it before the
// reconcile that applies the rename.
func (s *Supervisor) RenamedRef(kind, old, name string) {
	rekey := func(key string) string {
		nk, changed := xray.RenameDialerRef(key, kind, old, name)
		if !changed {
			return key
		}
		refs, err := xray.ParseDialer(nk)
		if err != nil {
			return key
		}
		return xray.DialerGroupKey(refs)
	}
	oldMember, newMember := "", ""
	if kind == xray.RefXray {
		oldMember, newMember = "xray-"+xray.NormalizeName(old), "xray-"+name
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	idx := make(map[string]int, len(s.slotIdx))
	for k, v := range s.slotIdx {
		idx[rekey(k)] = v
	}
	s.slotIdx = idx
	slots := make([]xray.Slot, len(s.loadedSlots))
	for i, sl := range s.loadedSlots {
		sl.Key = rekey(sl.Key)
		if oldMember != "" {
			sl.Picked = renamedKeys(sl.Picked, oldMember, newMember)
			for _, m := range sl.Members {
				if m.Key == oldMember {
					s.picker.rename(xray.SlotMemberTag(sl.Index, oldMember), xray.SlotMemberTag(sl.Index, newMember))
				}
			}
		}
		// Per-master picks: keyed by master name, holding member keys.
		if len(sl.MasterPicks) > 0 {
			mp := make(map[string]xray.MasterPick, len(sl.MasterPicks))
			for master, p := range sl.MasterPicks {
				if kind == xray.RefXray && master == xray.NormalizeName(old) {
					master = name
				}
				if oldMember != "" {
					p.Picked = renamedKeys(p.Picked, oldMember, newMember)
				}
				mp[master] = p
			}
			sl.MasterPicks = mp
		}
		slots[i] = sl
	}
	s.loadedSlots = slots
	if oldMember != "" {
		s.parker.rename(oldMember, newMember)
		s.chain.renameMember(oldMember, newMember)
	}
	if kind == xray.RefXray {
		s.chain.renameMaster(xray.NormalizeName(old), name)
	}
	s.history.rename(rekey, oldMember, newMember)
	s.auto.rename(rekey)
}

// renamedKeys returns keys with old replaced by key, as a copy.
func renamedKeys(keys []string, old, key string) []string {
	out := append([]string(nil), keys...)
	for i, k := range out {
		if k == old {
			out[i] = key
		}
	}
	return out
}

// rename moves a member's score to its new tag.
func (p *nodePicker) rename(oldTag, newTag string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if sc, ok := p.scores[oldTag]; ok {
		delete(p.scores, oldTag)
		p.scores[newTag] = sc
	}
}

// rename moves a member's park state to its new key.
func (p *nodeParker) rename(oldKey, newKey string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.nodes[oldKey]; ok {
		delete(p.nodes, oldKey)
		p.nodes[newKey] = st
	}
}
