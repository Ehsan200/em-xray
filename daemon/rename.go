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
			picked := append([]string(nil), sl.Picked...)
			for j, k := range picked {
				if k == oldMember {
					picked[j] = newMember
					s.picker.rename(xray.SlotMemberTag(sl.Index, oldMember), xray.SlotMemberTag(sl.Index, newMember))
				}
			}
			sl.Picked = picked
		}
		slots[i] = sl
	}
	s.loadedSlots = slots
	if oldMember != "" {
		s.parker.rename(oldMember, newMember)
	}
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
