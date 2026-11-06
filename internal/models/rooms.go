package models

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"kam.bytecoder.nl/internal/mesh"
)

const (
	// MaxRooms bounds the rooms one KAM instance runs; every room shares one
	// radio and one airtime budget (docs/FeatureRoomServer.md RS-MULTI-5).
	MaxRooms = 10
	// MaxPasswordBytes is the longest password MeshCore clients send; they
	// truncate at 15 bytes, so a longer configured password could never match.
	MaxPasswordBytes = 15
	// ReferenceAdminPassword is the MeshCore firmware default and is refused.
	ReferenceAdminPassword = "password"
)

// MeshConfig holds the radio-wide settings shared by all rooms.
type MeshConfig struct {
	// PathHashSize is the bytes per path hash on flood packets KAM originates
	// (1-3). Firmware 1.13 and older drop multi-byte hashes, so the default is 1.
	PathHashSize int `yaml:"path_hash_size"`
	// DefaultScope is the region used for adverts and for replies whose request
	// scope is unknown. Empty means unscoped floods.
	DefaultScope string `yaml:"default_scope,omitempty"`
	// Regions are the public (hashtag) regions KAM recognises on inbound floods.
	Regions []string `yaml:"regions,omitempty"`
	// DutyCyclePercent caps estimated airtime per hour for deferrable traffic
	// (pushes and adverts); 0 disables the cap.
	DutyCyclePercent float64 `yaml:"duty_cycle_percent"`
}

// DefaultMeshConfig returns the settings used when kam.yml has no mesh block.
func DefaultMeshConfig() MeshConfig {
	return MeshConfig{PathHashSize: 1, DutyCyclePercent: 10}
}

// UnmarshalYAML keeps defaults for keys the file omits.
func (m *MeshConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain MeshConfig
	p := plain(DefaultMeshConfig())
	if err := node.Decode(&p); err != nil {
		return err
	}
	*m = MeshConfig(p)
	return nil
}

var regionNamePattern = regexp.MustCompile(`^#?[A-Za-z0-9][A-Za-z0-9-]{0,29}$`)

// Validate checks the shared settings.
func (m MeshConfig) Validate() error {
	if m.PathHashSize < 1 || m.PathHashSize > 3 {
		return fmt.Errorf("mesh.path_hash_size must be 1, 2 or 3, got %d", m.PathHashSize)
	}
	if m.DutyCyclePercent < 0 || m.DutyCyclePercent > 100 {
		return fmt.Errorf("mesh.duty_cycle_percent must be between 0 and 100, got %v", m.DutyCyclePercent)
	}
	if m.DefaultScope != "" && !regionNamePattern.MatchString(m.DefaultScope) {
		return fmt.Errorf("mesh.default_scope %q is not a valid public region name", m.DefaultScope)
	}
	for _, region := range m.Regions {
		if !regionNamePattern.MatchString(region) {
			return fmt.Errorf("mesh.regions entry %q is not a valid public region name (private '$' regions are not supported)", region)
		}
	}
	return nil
}

// KnownRegions returns the default scope plus the configured regions without
// duplicates, in a stable order.
func (m MeshConfig) KnownRegions() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range append([]string{m.DefaultScope}, m.Regions...) {
		key := strings.TrimPrefix(name, "#")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// LocationConfig is a node position advertised to clients.
type LocationConfig struct {
	Latitude  float64 `yaml:"latitude"`
	Longitude float64 `yaml:"longitude"`
}

// RoomConfig describes one MeshCore room server hosted by KAM.
type RoomConfig struct {
	ID       string          `yaml:"id"`
	Name     string          `yaml:"name"`
	Enabled  bool            `yaml:"enabled"`
	Location *LocationConfig `yaml:"location,omitempty"`

	// AdminPassword grants the ADMIN role. Like channel keys, passwords are
	// plain text in kam.yml (docs/FeatureRoomServer.md D11).
	AdminPassword string `yaml:"admin_password"`
	// GuestPassword grants READ_WRITE. It may only be empty together with
	// OpenRoom.
	GuestPassword string `yaml:"guest_password,omitempty"`
	// OpenRoom lets anyone with a blank password log in as READ_WRITE.
	OpenRoom bool `yaml:"open_room,omitempty"`
	// AllowReadOnly lets any other password log in as GUEST, who receives
	// posts but cannot post.
	AllowReadOnly bool `yaml:"allow_read_only,omitempty"`

	MaxClients               int `yaml:"max_clients"`
	AdvertIntervalMin        int `yaml:"advert_interval_min"`
	FloodAdvertIntervalHours int `yaml:"flood_advert_interval_hours"`
	MaxPosts                 int `yaml:"max_posts"`
	MaxAgeDays               int `yaml:"max_age_days"`
	BacklogPosts             int `yaml:"backlog_posts"`
}

// DefaultRoomConfig returns the values used for keys a room omits.
func DefaultRoomConfig() RoomConfig {
	return RoomConfig{
		Enabled:                  true,
		MaxClients:               20,
		AdvertIntervalMin:        2,
		FloodAdvertIntervalHours: 47,
		MaxPosts:                 1000,
		MaxAgeDays:               30,
		BacklogPosts:             32,
	}
}

// UnmarshalYAML keeps defaults for keys the room omits.
func (r *RoomConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain RoomConfig
	p := plain(DefaultRoomConfig())
	if err := node.Decode(&p); err != nil {
		return err
	}
	*r = RoomConfig(p)
	return nil
}

var roomIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate checks one room. A room that fails stays disabled; see ResolveRooms.
func (r RoomConfig) Validate() error {
	if !roomIDPattern.MatchString(r.ID) {
		return fmt.Errorf("id %q must be 1-32 characters of a-z, 0-9, '-' or '_', starting with a letter or digit", r.ID)
	}
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name is required")
	}
	limit := mesh.MaxAdvertNameLen(r.Location != nil)
	if len(r.Name) > limit {
		return fmt.Errorf("name is %d bytes, the advert allows %d (fewer when a location is set)", len(r.Name), limit)
	}
	if r.Location != nil {
		if r.Location.Latitude < -90 || r.Location.Latitude > 90 || r.Location.Longitude < -180 || r.Location.Longitude > 180 {
			return fmt.Errorf("location %v,%v is out of range", r.Location.Latitude, r.Location.Longitude)
		}
	}

	if r.AdminPassword == "" {
		return fmt.Errorf("admin_password is required")
	}
	if r.AdminPassword == ReferenceAdminPassword {
		return fmt.Errorf("admin_password must not be the MeshCore default %q", ReferenceAdminPassword)
	}
	for name, password := range map[string]string{"admin_password": r.AdminPassword, "guest_password": r.GuestPassword} {
		if len(password) > MaxPasswordBytes {
			return fmt.Errorf("%s is %d bytes; MeshCore clients send at most %d", name, len(password), MaxPasswordBytes)
		}
		for _, b := range []byte(password) {
			if b < 0x20 || b == 0x7F {
				return fmt.Errorf("%s contains a control character", name)
			}
		}
	}
	if r.GuestPassword != "" && r.GuestPassword == r.AdminPassword {
		return fmt.Errorf("guest_password must differ from admin_password")
	}
	if r.OpenRoom && r.GuestPassword != "" {
		return fmt.Errorf("open_room requires an empty guest_password")
	}

	if r.MaxClients < 1 || r.MaxClients > 100 {
		return fmt.Errorf("max_clients must be between 1 and 100, got %d", r.MaxClients)
	}
	if r.AdvertIntervalMin != 0 && (r.AdvertIntervalMin < 1 || r.AdvertIntervalMin > 1440) {
		return fmt.Errorf("advert_interval_min must be 0 (off) or between 1 and 1440, got %d", r.AdvertIntervalMin)
	}
	if r.FloodAdvertIntervalHours != 0 && (r.FloodAdvertIntervalHours < 3 || r.FloodAdvertIntervalHours > 168) {
		return fmt.Errorf("flood_advert_interval_hours must be 0 (off) or between 3 and 168, got %d", r.FloodAdvertIntervalHours)
	}
	if r.BacklogPosts < 1 || r.BacklogPosts > 256 {
		return fmt.Errorf("backlog_posts must be between 1 and 256, got %d", r.BacklogPosts)
	}
	if r.MaxPosts < 0 || (r.MaxPosts > 0 && r.MaxPosts < r.BacklogPosts) {
		return fmt.Errorf("max_posts must be 0 (unlimited) or at least backlog_posts (%d), got %d", r.BacklogPosts, r.MaxPosts)
	}
	if r.MaxAgeDays < 0 {
		return fmt.Errorf("max_age_days must not be negative, got %d", r.MaxAgeDays)
	}
	return nil
}

// ResolveRooms validates the mesh and room settings and returns the rooms that
// may run. Problems never fail the whole configuration: an invalid room stays
// disabled, an invalid mesh block disables every room, and the radio keeps
// ingesting either way (RS-CFG-1, RS-OPS-4).
func (cfg *KamConfig) ResolveRooms() (MeshConfig, []RoomConfig, []error) {
	meshCfg := DefaultMeshConfig()
	if cfg.Mesh != nil {
		meshCfg = *cfg.Mesh
	}
	var problems []error
	if err := meshCfg.Validate(); err != nil {
		if len(cfg.Rooms) > 0 {
			problems = append(problems, fmt.Errorf("rooms disabled: %w", err))
		}
		return meshCfg, nil, problems
	}

	var rooms []RoomConfig
	seen := map[string]bool{}
	for index, room := range cfg.Rooms {
		if !room.Enabled {
			continue
		}
		room.ID = strings.TrimSpace(room.ID)
		room.Name = strings.TrimSpace(room.Name)
		label := room.ID
		if label == "" {
			label = fmt.Sprintf("#%d", index+1)
		}
		if err := room.Validate(); err != nil {
			problems = append(problems, fmt.Errorf("room %s disabled: %w", label, err))
			continue
		}
		if seen[room.ID] {
			problems = append(problems, fmt.Errorf("room %s disabled: duplicate id", label))
			continue
		}
		if len(rooms) >= MaxRooms {
			problems = append(problems, fmt.Errorf("room %s disabled: at most %d rooms may run", label, MaxRooms))
			continue
		}
		seen[room.ID] = true
		rooms = append(rooms, room)
	}
	return meshCfg, rooms, problems
}
