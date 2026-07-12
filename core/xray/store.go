package xray

import (
	"errors"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// ErrNotFound is returned by Get* helpers when no row matches.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed persistence layer for entries, subscriptions,
// nodes and overrides. Pure Go (glebarez/modernc sqlite) so it cross-compiles
// without CGO.
type Store struct {
	db *gorm.DB
}

// Open opens (creating if absent) the database at dsn and migrates the schema.
// dsn is a file path, or ":memory:" for tests.
func Open(dsn string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	// WAL + a busy timeout so concurrent daemon goroutines don't trip "locked".
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if err := db.Exec(p).Error; err != nil {
			return nil, err
		}
	}
	if err := db.AutoMigrate(&Subscription{}, &SubNode{}, &SubNodeOverride{}, &XrayEntry{}, &Inbound{}); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// DB exposes the gorm handle for higher layers that need raw access.
func (s *Store) DB() *gorm.DB { return s.db }

// ---- Subscriptions ---------------------------------------------------------

// CreateSubscription validates + inserts a subscription. Name must be unique.
func (s *Store) CreateSubscription(sub *Subscription) error {
	name, err := ValidName(sub.Name)
	if err != nil {
		return err
	}
	sub.Name = name
	if sub.UserAgent == "" {
		sub.UserAgent = DefaultSubUserAgent
	}
	return s.db.Create(sub).Error
}

func (s *Store) GetSubscription(id uint) (*Subscription, error) {
	var sub Subscription
	err := s.db.First(&sub, id).Error
	return one(&sub, err)
}

func (s *Store) GetSubscriptionByName(name string) (*Subscription, error) {
	var sub Subscription
	err := s.db.Where("name = ?", NormalizeName(name)).First(&sub).Error
	return one(&sub, err)
}

func (s *Store) ListSubscriptions() ([]Subscription, error) {
	var subs []Subscription
	err := s.db.Order("name").Find(&subs).Error
	return subs, err
}

// UpdateSubscription saves all fields of an existing subscription.
func (s *Store) UpdateSubscription(sub *Subscription) error {
	return s.db.Save(sub).Error
}

// SetSubEnabled toggles a subscription and recomputes its nodes' active flags.
func (s *Store) SetSubEnabled(id uint, enabled bool) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Subscription{}).Where("id = ?", id).Update("enabled", enabled).Error; err != nil {
			return err
		}
		return recomputeActive(tx, id)
	})
}

// DeleteSubscription removes a subscription and all its nodes + overrides.
func (s *Store) DeleteSubscription(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sub_id = ?", id).Delete(&SubNode{}).Error; err != nil {
			return err
		}
		if err := tx.Where("sub_id = ?", id).Delete(&SubNodeOverride{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Subscription{}, id).Error
	})
}

// ---- Nodes -----------------------------------------------------------------

// ReplaceNodes atomically swaps the entire node set for a subscription (volatile
// semantics) and recomputes active flags. Durable overrides are untouched, so a
// prior manual disable still applies to any node whose fingerprint returns.
func (s *Store) ReplaceNodes(subID uint, nodes []SubNode) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("sub_id = ?", subID).Delete(&SubNode{}).Error; err != nil {
			return err
		}
		for i := range nodes {
			nodes[i].ID = 0 // fresh autoincrement => ascending == insertion order
			nodes[i].SubID = subID
			if err := tx.Create(&nodes[i]).Error; err != nil {
				return err
			}
		}
		return recomputeActive(tx, subID)
	})
}

// NodesForSub returns all nodes for a subscription in insertion order.
func (s *Store) NodesForSub(subID uint) ([]SubNode, error) {
	var ns []SubNode
	err := s.db.Where("sub_id = ?", subID).Order("id").Find(&ns).Error
	return ns, err
}

// ActiveNodes returns only the active nodes for a subscription, in order.
func (s *Store) ActiveNodes(subID uint) ([]SubNode, error) {
	var ns []SubNode
	err := s.db.Where("sub_id = ? AND active = ?", subID, true).Order("id").Find(&ns).Error
	return ns, err
}

// NodeNameByFingerprint returns a node's display name for a fingerprint, or the
// fingerprint itself if no such node is stored.
func (s *Store) NodeNameByFingerprint(fp string) string {
	var n SubNode
	if err := s.db.Where("fingerprint = ?", fp).First(&n).Error; err == nil {
		return n.Name
	}
	return fp
}

// DisabledFingerprints returns the set of node fingerprints a subscription has a
// durable disable-override for.
func (s *Store) DisabledFingerprints(subID uint) (map[string]bool, error) {
	var overrides []SubNodeOverride
	if err := s.db.Where("sub_id = ? AND disabled = ?", subID, true).Find(&overrides).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		out[o.Fingerprint] = true
	}
	return out, nil
}

// SetNodeDisabled upserts a durable override and recomputes active flags.
func (s *Store) SetNodeDisabled(subID uint, fingerprint string, disabled bool) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		ov := SubNodeOverride{SubID: subID, Fingerprint: fingerprint, Disabled: disabled}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sub_id"}, {Name: "fingerprint"}},
			DoUpdates: clause.AssignmentColumns([]string{"disabled"}),
		}).Create(&ov).Error; err != nil {
			return err
		}
		return recomputeActive(tx, subID)
	})
}

// recomputeActive derives each node's Active flag (never set directly):
// active = sub enabled AND not overridden-off AND within the first EffectiveCap()
// non-disabled nodes in insertion order. Overridden-off nodes do not consume cap.
func recomputeActive(tx *gorm.DB, subID uint) error {
	var sub Subscription
	if err := tx.First(&sub, subID).Error; err != nil {
		return err
	}
	var nodes []SubNode
	if err := tx.Where("sub_id = ?", subID).Order("id").Find(&nodes).Error; err != nil {
		return err
	}
	var overrides []SubNodeOverride
	if err := tx.Where("sub_id = ?", subID).Find(&overrides).Error; err != nil {
		return err
	}
	off := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		if o.Disabled {
			off[o.Fingerprint] = true
		}
	}

	capN := sub.EffectiveCap()
	kept := 0
	for i := range nodes {
		want := false
		if sub.Enabled && !off[nodes[i].Fingerprint] {
			if kept < capN {
				want = true
				kept++
			}
		}
		if nodes[i].Active != want {
			if err := tx.Model(&SubNode{}).Where("id = ?", nodes[i].ID).Update("active", want).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- Entries ---------------------------------------------------------------

func (s *Store) CreateEntry(e *XrayEntry) error {
	name, err := ValidName(e.Name)
	if err != nil {
		return err
	}
	e.Name = name
	return s.db.Create(e).Error
}

func (s *Store) GetEntry(id uint) (*XrayEntry, error) {
	var e XrayEntry
	err := s.db.First(&e, id).Error
	return one(&e, err)
}

func (s *Store) GetEntryByName(name string) (*XrayEntry, error) {
	var e XrayEntry
	err := s.db.Where("name = ?", NormalizeName(name)).First(&e).Error
	return one(&e, err)
}

func (s *Store) ListEntries() ([]XrayEntry, error) {
	var es []XrayEntry
	err := s.db.Order("name").Find(&es).Error
	return es, err
}

func (s *Store) UpdateEntry(e *XrayEntry) error { return s.db.Save(e).Error }

func (s *Store) DeleteEntry(id uint) error { return s.db.Delete(&XrayEntry{}, id).Error }

// RenameEntry renames an entry and cascades the change into every master's
// Dialer that references it (xray:old → xray:new), atomically.
func (s *Store) RenameEntry(id uint, newName string) error {
	name, err := ValidName(newName)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var e XrayEntry
		if err := tx.First(&e, id).Error; err != nil {
			return err
		}
		old := e.Name
		if old == name {
			return nil
		}
		if err := tx.Model(&XrayEntry{}).Where("id = ?", id).Update("name", name).Error; err != nil {
			return err
		}
		return cascadeRename(tx, RefXray, old, name)
	})
}

// RenameSubscription renames a subscription and cascades into every master's
// Dialer that references it (xraysub:old → xraysub:new), atomically.
func (s *Store) RenameSubscription(id uint, newName string) error {
	name, err := ValidName(newName)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var sub Subscription
		if err := tx.First(&sub, id).Error; err != nil {
			return err
		}
		old := sub.Name
		if old == name {
			return nil
		}
		if err := tx.Model(&Subscription{}).Where("id = ?", id).Update("name", name).Error; err != nil {
			return err
		}
		return cascadeRename(tx, RefXraySub, old, name)
	})
}

// cascadeRename rewrites the given ref kind old→new across every master's Dialer.
func cascadeRename(tx *gorm.DB, kind, old, name string) error {
	var masters []XrayEntry
	if err := tx.Where("dialer <> ''").Find(&masters).Error; err != nil {
		return err
	}
	for _, m := range masters {
		nd, changed := RenameDialerRef(m.Dialer, kind, old, name)
		if changed {
			if err := tx.Model(&XrayEntry{}).Where("id = ?", m.ID).Update("dialer", nd).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// NamesExist reports whether every given entry name exists.
func (s *Store) NamesExist(names []string) (missing []string) {
	for _, n := range names {
		if _, err := s.GetEntryByName(n); err != nil {
			missing = append(missing, n)
		}
	}
	return missing
}

// SubNamesExist reports whether every given subscription name exists.
func (s *Store) SubNamesExist(names []string) (missing []string) {
	for _, n := range names {
		if _, err := s.GetSubscriptionByName(n); err != nil {
			missing = append(missing, n)
		}
	}
	return missing
}

func (s *Store) SetEntryEnabled(id uint, enabled bool) error {
	return s.db.Model(&XrayEntry{}).Where("id = ?", id).Update("enabled", enabled).Error
}

// ---- Inbounds --------------------------------------------------------------

// CreateInbound validates + inserts an inbound, assigning a free listen port
// when none was set.
func (s *Store) CreateInbound(in *Inbound) error {
	name, err := ValidName(in.Name)
	if err != nil {
		return err
	}
	in.Name = name
	if _, _, err := ParseTarget(in.Target); err != nil {
		return err
	}
	if in.Port == 0 {
		if err := s.assignInboundPort(in); err != nil {
			return err
		}
	}
	return s.db.Create(in).Error
}

// assignInboundPort picks the lowest free port in range not used by any inbound.
func (s *Store) assignInboundPort(in *Inbound) error {
	var existing []Inbound
	if err := s.db.Find(&existing).Error; err != nil {
		return err
	}
	used := make(map[int]bool)
	for _, e := range existing {
		if e.Port != 0 {
			used[e.Port] = true
		}
	}
	p := nextFreePort(used)
	if p == 0 {
		return errors.New("no free inbound ports in range")
	}
	in.Port = p
	return nil
}

func (s *Store) GetInbound(id uint) (*Inbound, error) {
	var in Inbound
	err := s.db.First(&in, id).Error
	return one(&in, err)
}

func (s *Store) GetInboundByName(name string) (*Inbound, error) {
	var in Inbound
	err := s.db.Where("name = ?", NormalizeName(name)).First(&in).Error
	return one(&in, err)
}

func (s *Store) ListInbounds() ([]Inbound, error) {
	var ins []Inbound
	err := s.db.Order("name").Find(&ins).Error
	return ins, err
}

func (s *Store) UpdateInbound(in *Inbound) error { return s.db.Save(in).Error }

func (s *Store) DeleteInbound(id uint) error { return s.db.Delete(&Inbound{}, id).Error }

func (s *Store) SetInboundEnabled(id uint, enabled bool) error {
	return s.db.Model(&Inbound{}).Where("id = ?", id).Update("enabled", enabled).Error
}

// one maps gorm's ErrRecordNotFound to ErrNotFound and returns the row otherwise.
func one[T any](v *T, err error) (*T, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}
