package xray

import "fmt"

// KindUser is the traffic kind for per-user (per-client) counters.
const KindUser = "user"

// UserEmail builds the stable xray stats email/tag for an inbound user. It must
// be unique across the whole config, so it namespaces the user under the
// inbound. xray reports its bytes as user>>>EMAIL>>>traffic>>>{up,down}link.
func UserEmail(inboundName, userName string) string {
	return sanitizeKey(inboundName) + "." + sanitizeKey(userName)
}

// PrimaryUserEmail is the stats email for an inbound's own (primary) credential.
func PrimaryUserEmail(inboundName string) string {
	return sanitizeKey(inboundName)
}

// NewInboundUser builds an additional client for an inbound, generating the
// right credential for the protocol and a stats email. capBytes 0 = unlimited.
func NewInboundUser(in *Inbound, name string, capBytes int64) (*InboundUser, error) {
	n, err := ValidName(name)
	if err != nil {
		return nil, err
	}
	u := &InboundUser{
		InboundID: in.ID,
		Name:      n,
		Email:     UserEmail(in.Name, n),
		ByteCap:   capBytes,
		Enabled:   true,
	}
	switch in.Protocol {
	case "vless", "vmess":
		id, err := NewUUID()
		if err != nil {
			return nil, err
		}
		u.UUID = id
	case "trojan":
		p, err := NewPassword()
		if err != nil {
			return nil, err
		}
		u.Password = p
	case "hysteria":
		a, err := NewPassword()
		if err != nil {
			return nil, err
		}
		u.Auth = a
	default:
		return nil, fmt.Errorf("protocol %q does not support extra users", in.Protocol)
	}
	return u, nil
}

// CreateInboundUser validates + inserts an inbound user.
func (s *Store) CreateInboundUser(u *InboundUser) error {
	if _, err := ValidName(u.Name); err != nil {
		return err
	}
	return s.db.Create(u).Error
}

// InboundUsers returns the users of an inbound, oldest first.
func (s *Store) InboundUsers(inboundID uint) ([]InboundUser, error) {
	var us []InboundUser
	err := s.db.Where("inbound_id = ?", inboundID).Order("id").Find(&us).Error
	return us, err
}

// DeleteInboundUser removes a user by id.
func (s *Store) DeleteInboundUser(id uint) error {
	return s.db.Delete(&InboundUser{}, id).Error
}

// GetInboundUser fetches a user by id.
func (s *Store) GetInboundUser(id uint) (*InboundUser, error) {
	var u InboundUser
	err := s.db.First(&u, id).Error
	return one(&u, err)
}

// SetInboundUserEnabled toggles a user.
func (s *Store) SetInboundUserEnabled(id uint, enabled bool) error {
	return s.db.Model(&InboundUser{}).Where("id = ?", id).Update("enabled", enabled).Error
}

// TrafficTotalFor returns the lifetime up/down for a (kind, tag), or zeros.
func (s *Store) TrafficTotalFor(kind, tag string) (up, down int64) {
	var t TrafficTotal
	if err := s.db.Where("kind = ? AND tag = ?", kind, tag).First(&t).Error; err != nil {
		return 0, 0
	}
	return t.Up, t.Down
}
