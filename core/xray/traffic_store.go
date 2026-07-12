package xray

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AddTraffic accumulates a byte delta for (kind, tag) into the given hour bucket
// and into the lifetime total, in one transaction. name refreshes the total's
// display label. A zero delta still refreshes the name (so a tag with no new
// traffic keeps a current label).
func (s *Store) AddTraffic(kind, tag, name string, hourUnix, up, down int64) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		bucket := TrafficBucket{Kind: kind, Tag: tag, HourUnix: hourUnix, Up: up, Down: down}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "kind"}, {Name: "tag"}, {Name: "hour_unix"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up":   gorm.Expr("up + ?", up),
				"down": gorm.Expr("down + ?", down),
			}),
		}).Create(&bucket).Error; err != nil {
			return err
		}
		total := TrafficTotal{Kind: kind, Tag: tag, Name: name, Up: up, Down: down, UpdatedAt: time.Now()}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "kind"}, {Name: "tag"}},
			DoUpdates: clause.Assignments(map[string]any{
				"up":         gorm.Expr("up + ?", up),
				"down":       gorm.Expr("down + ?", down),
				"name":       name,
				"updated_at": total.UpdatedAt,
			}),
		}).Create(&total).Error
	})
}

// TrafficTotals returns every lifetime total.
func (s *Store) TrafficTotals() ([]TrafficTotal, error) {
	var ts []TrafficTotal
	err := s.db.Order("kind, name").Find(&ts).Error
	return ts, err
}

// TrafficBucketsSince returns the hour buckets for (kind, tag) with HourUnix >=
// since, ascending by hour.
func (s *Store) TrafficBucketsSince(kind, tag string, since int64) ([]TrafficBucket, error) {
	var bs []TrafficBucket
	err := s.db.Where("kind = ? AND tag = ? AND hour_unix >= ?", kind, tag, since).
		Order("hour_unix").Find(&bs).Error
	return bs, err
}

// PruneTrafficBefore deletes hour buckets older than hourUnix. Lifetime totals
// are untouched.
func (s *Store) PruneTrafficBefore(hourUnix int64) error {
	return s.db.Where("hour_unix < ?", hourUnix).Delete(&TrafficBucket{}).Error
}
