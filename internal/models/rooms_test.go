package models

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const roomTestBase = `
version: 1
node_name: "kam"
radio:
  port: "/dev/ttyUSB0"
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels:
  - name: "#public"
    key_strategy: "public"
    key_hex: "8b3387e9c5cdea6ac9e5edbaa115cd72"
    enabled: true
`

func parseRoomConfig(t *testing.T, extra string) *KamConfig {
	t.Helper()
	cfg, err := ParseKamConfig([]byte(roomTestBase + extra))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func validRoom() RoomConfig {
	room := DefaultRoomConfig()
	room.ID, room.Name, room.AdminPassword = "lobby", "KAM Lobby", "s3cret"
	return room
}

func TestRoomsAreOptional(t *testing.T) {
	cfg := parseRoomConfig(t, "")
	if cfg.Mesh != nil || len(cfg.Rooms) != 0 {
		t.Fatalf("unexpected mesh/rooms: %+v %+v", cfg.Mesh, cfg.Rooms)
	}
	meshCfg, rooms, problems := cfg.ResolveRooms()
	if len(rooms) != 0 || len(problems) != 0 {
		t.Fatalf("rooms=%v problems=%v", rooms, problems)
	}
	if !reflect.DeepEqual(meshCfg, DefaultMeshConfig()) {
		t.Fatalf("mesh defaults: %+v", meshCfg)
	}
}

func TestRoomDefaultsApplyToOmittedKeys(t *testing.T) {
	cfg := parseRoomConfig(t, `
rooms:
  - id: lobby
    name: "KAM Lobby"
    admin_password: "s3cret"
`)
	_, rooms, problems := cfg.ResolveRooms()
	if len(problems) != 0 || len(rooms) != 1 {
		t.Fatalf("rooms=%v problems=%v", rooms, problems)
	}
	want := validRoom()
	if rooms[0] != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", rooms[0], want)
	}
	if !rooms[0].Enabled || rooms[0].MaxClients != 20 || rooms[0].AdvertIntervalMin != 2 || rooms[0].FloodAdvertIntervalHours != 47 ||
		rooms[0].MaxPosts != 1000 || rooms[0].MaxAgeDays != 30 || rooms[0].BacklogPosts != 32 {
		t.Fatalf("documented defaults changed: %+v", rooms[0])
	}
}

func TestRoomExplicitZeroValuesAreKept(t *testing.T) {
	cfg := parseRoomConfig(t, `
mesh:
  duty_cycle_percent: 0
rooms:
  - id: quiet
    name: "Quiet"
    admin_password: "s3cret"
    advert_interval_min: 0
    flood_advert_interval_hours: 0
    max_posts: 0
    max_age_days: 0
`)
	meshCfg, rooms, problems := cfg.ResolveRooms()
	if len(problems) != 0 || len(rooms) != 1 {
		t.Fatalf("rooms=%v problems=%v", rooms, problems)
	}
	if meshCfg.DutyCyclePercent != 0 || meshCfg.PathHashSize != 1 {
		t.Fatalf("mesh: %+v", meshCfg)
	}
	r := rooms[0]
	if r.AdvertIntervalMin != 0 || r.FloodAdvertIntervalHours != 0 || r.MaxPosts != 0 || r.MaxAgeDays != 0 {
		t.Fatalf("explicit zeros replaced by defaults: %+v", r)
	}
}

func TestRoomEnabledFalseIsHonoured(t *testing.T) {
	cfg := parseRoomConfig(t, `
rooms:
  - id: off
    name: "Off"
    enabled: false
    admin_password: ""
`)
	_, rooms, problems := cfg.ResolveRooms()
	if len(rooms) != 0 || len(problems) != 0 {
		t.Fatalf("a disabled room must be skipped silently: rooms=%v problems=%v", rooms, problems)
	}
}

func TestRoomValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RoomConfig)
		want   string
	}{
		{"valid", func(*RoomConfig) {}, ""},
		{"empty id", func(r *RoomConfig) { r.ID = "" }, "id"},
		{"upper-case id", func(r *RoomConfig) { r.ID = "Lobby" }, "id"},
		{"long id", func(r *RoomConfig) { r.ID = strings.Repeat("a", 33) }, "id"},
		{"empty name", func(r *RoomConfig) { r.Name = "  " }, "name is required"},
		{"name too long", func(r *RoomConfig) { r.Name = strings.Repeat("n", 32) }, "advert allows 31"},
		{"name too long with location", func(r *RoomConfig) {
			r.Name = strings.Repeat("n", 24)
			r.Location = &LocationConfig{Latitude: 1, Longitude: 2}
		}, "advert allows 23"},
		{"name fits with location", func(r *RoomConfig) {
			r.Name = strings.Repeat("n", 23)
			r.Location = &LocationConfig{Latitude: 1, Longitude: 2}
		}, ""},
		{"latitude out of range", func(r *RoomConfig) { r.Location = &LocationConfig{Latitude: 91} }, "out of range"},
		{"longitude out of range", func(r *RoomConfig) { r.Location = &LocationConfig{Longitude: -181} }, "out of range"},
		{"empty admin password", func(r *RoomConfig) { r.AdminPassword = "" }, "admin_password is required"},
		{"reference admin password", func(r *RoomConfig) { r.AdminPassword = "password" }, "MeshCore default"},
		{"admin password too long", func(r *RoomConfig) { r.AdminPassword = strings.Repeat("p", 16) }, "at most 15"},
		{"admin password of exactly 15 bytes", func(r *RoomConfig) { r.AdminPassword = strings.Repeat("p", 15) }, ""},
		{"password with control character", func(r *RoomConfig) { r.AdminPassword = "pa\x00ss" }, "control character"},
		{"guest password too long", func(r *RoomConfig) { r.GuestPassword = strings.Repeat("g", 16) }, "guest_password"},
		{"guest equals admin", func(r *RoomConfig) { r.GuestPassword = r.AdminPassword }, "must differ"},
		{"open room with guest password", func(r *RoomConfig) { r.OpenRoom, r.GuestPassword = true, "guest" }, "open_room requires"},
		{"open room", func(r *RoomConfig) { r.OpenRoom = true }, ""},
		{"read-only guests", func(r *RoomConfig) { r.AllowReadOnly = true }, ""},
		{"no clients", func(r *RoomConfig) { r.MaxClients = 0 }, "max_clients"},
		{"too many clients", func(r *RoomConfig) { r.MaxClients = 101 }, "max_clients"},
		{"advert interval too long", func(r *RoomConfig) { r.AdvertIntervalMin = 1441 }, "advert_interval_min"},
		{"negative advert interval", func(r *RoomConfig) { r.AdvertIntervalMin = -1 }, "advert_interval_min"},
		{"flood advert too often", func(r *RoomConfig) { r.FloodAdvertIntervalHours = 2 }, "flood_advert_interval_hours"},
		{"flood advert too rarely", func(r *RoomConfig) { r.FloodAdvertIntervalHours = 169 }, "flood_advert_interval_hours"},
		{"backlog zero", func(r *RoomConfig) { r.BacklogPosts = 0 }, "backlog_posts"},
		{"backlog too large", func(r *RoomConfig) { r.BacklogPosts = 257 }, "backlog_posts"},
		{"max posts below backlog", func(r *RoomConfig) { r.MaxPosts = 10 }, "at least backlog_posts"},
		{"negative max posts", func(r *RoomConfig) { r.MaxPosts = -1 }, "max_posts"},
		{"negative max age", func(r *RoomConfig) { r.MaxAgeDays = -1 }, "max_age_days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room := validRoom()
			tc.mutate(&room)
			err := room.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMeshValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MeshConfig)
		want   string
	}{
		{"defaults", func(*MeshConfig) {}, ""},
		{"hash size 0", func(m *MeshConfig) { m.PathHashSize = 0 }, "path_hash_size"},
		{"hash size 4", func(m *MeshConfig) { m.PathHashSize = 4 }, "path_hash_size"},
		{"hash size 3", func(m *MeshConfig) { m.PathHashSize = 3 }, ""},
		{"negative duty cycle", func(m *MeshConfig) { m.DutyCyclePercent = -1 }, "duty_cycle_percent"},
		{"duty cycle over 100", func(m *MeshConfig) { m.DutyCyclePercent = 100.5 }, "duty_cycle_percent"},
		{"hashtag scope", func(m *MeshConfig) { m.DefaultScope = "#europe" }, ""},
		{"bare scope", func(m *MeshConfig) { m.DefaultScope = "europe-west" }, ""},
		{"scope with space", func(m *MeshConfig) { m.DefaultScope = "eu rope" }, "default_scope"},
		{"private region", func(m *MeshConfig) { m.Regions = []string{"$secret"} }, "regions entry"},
		{"empty region", func(m *MeshConfig) { m.Regions = []string{""} }, "regions entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := DefaultMeshConfig()
			tc.mutate(&m)
			err := m.Validate()
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestKnownRegionsDeduplicatesAndKeepsDefaultFirst(t *testing.T) {
	m := MeshConfig{DefaultScope: "#eu", Regions: []string{"eu", "#uk", "uk", "de"}}
	got := m.KnownRegions()
	if strings.Join(got, ",") != "#eu,#uk,de" {
		t.Fatalf("KnownRegions = %v", got)
	}
	if len((MeshConfig{}).KnownRegions()) != 0 {
		t.Fatal("empty config must have no regions")
	}
}

func TestResolveRoomsDisablesOnlyTheInvalidRooms(t *testing.T) {
	cfg := parseRoomConfig(t, `
rooms:
  - id: good
    name: "Good"
    admin_password: "s3cret"
  - id: bad
    name: "Bad"
    admin_password: "password"
  - id: good
    name: "Duplicate"
    admin_password: "s3cret"
  - name: "No id"
    admin_password: "s3cret"
  - id: other
    name: "Other"
    admin_password: "an0ther"
`)
	_, rooms, problems := cfg.ResolveRooms()
	if len(rooms) != 2 || rooms[0].ID != "good" || rooms[1].ID != "other" {
		t.Fatalf("rooms: %+v", rooms)
	}
	if len(problems) != 3 {
		t.Fatalf("problems: %v", problems)
	}
	joined := ""
	for _, p := range problems {
		joined += p.Error() + "\n"
	}
	for _, want := range []string{"room bad disabled", "room good disabled: duplicate id", "room #4 disabled"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problems %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "s3cret") || strings.Contains(joined, "an0ther") {
		t.Fatalf("passwords leaked into problem text: %q", joined)
	}
}

func TestResolveRoomsLimitsRoomCount(t *testing.T) {
	var extra strings.Builder
	extra.WriteString("rooms:\n")
	ids := make([]string, 0, MaxRooms+1)
	for i := 1; i <= MaxRooms+1; i++ {
		ids = append(ids, fmt.Sprintf("r%d", i))
	}
	for _, id := range ids {
		extra.WriteString("  - id: " + id + "\n    name: " + id + "\n    admin_password: s3cret\n")
	}
	cfg := parseRoomConfig(t, extra.String())
	_, rooms, problems := cfg.ResolveRooms()
	if len(rooms) != MaxRooms || len(problems) != 1 || !strings.Contains(problems[0].Error(), fmt.Sprintf("room %s disabled", ids[len(ids)-1])) {
		t.Fatalf("rooms=%d problems=%v", len(rooms), problems)
	}
}

func TestResolveRoomsWithInvalidMeshDisablesAllRooms(t *testing.T) {
	cfg := parseRoomConfig(t, `
mesh:
  path_hash_size: 9
rooms:
  - id: lobby
    name: "Lobby"
    admin_password: "s3cret"
`)
	_, rooms, problems := cfg.ResolveRooms()
	if len(rooms) != 0 || len(problems) != 1 || !strings.Contains(problems[0].Error(), "path_hash_size") {
		t.Fatalf("rooms=%v problems=%v", rooms, problems)
	}

	// A bad mesh block without rooms is not worth a warning.
	cfg = parseRoomConfig(t, "mesh:\n  path_hash_size: 9\n")
	if _, rooms, problems := cfg.ResolveRooms(); len(rooms) != 0 || len(problems) != 0 {
		t.Fatalf("rooms=%v problems=%v", rooms, problems)
	}
}

func TestMeshBlockKeepsDefaultsForOmittedKeys(t *testing.T) {
	cfg := parseRoomConfig(t, `
mesh:
  default_scope: "#eu"
  regions: ["uk"]
rooms:
  - id: lobby
    name: "Lobby"
    admin_password: "s3cret"
`)
	meshCfg, _, problems := cfg.ResolveRooms()
	if len(problems) != 0 || meshCfg.PathHashSize != 1 || meshCfg.DutyCyclePercent != 10 || meshCfg.DefaultScope != "#eu" || len(meshCfg.Regions) != 1 {
		t.Fatalf("mesh: %+v problems=%v", meshCfg, problems)
	}
}

func TestSaveKamConfigKeepsRoomsAndMesh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.yml")
	original := parseRoomConfig(t, `
mesh:
  path_hash_size: 2
  default_scope: "#eu"
  duty_cycle_percent: 5
rooms:
  - id: lobby
    name: "Lobby"
    admin_password: "s3cret"
    guest_password: "guest"
    allow_read_only: true
    location:
      latitude: 52.5
      longitude: 5.25
    advert_interval_min: 0
    max_posts: 0
`)
	if err := SaveKamConfig(path, original); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadKamConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Mesh, original.Mesh) {
		t.Fatalf("mesh changed: %+v vs %+v", reloaded.Mesh, original.Mesh)
	}
	if len(reloaded.Rooms) != 1 || reloaded.Rooms[0].Location == nil || *reloaded.Rooms[0].Location != *original.Rooms[0].Location {
		t.Fatalf("rooms changed: %+v", reloaded.Rooms)
	}
	a, b := reloaded.Rooms[0], original.Rooms[0]
	a.Location, b.Location = nil, nil
	if a != b {
		t.Fatalf("room changed across save:\n got %+v\nwant %+v", a, b)
	}

	// A config without rooms must not grow a mesh or rooms block.
	plain := parseRoomConfig(t, "")
	if err := SaveKamConfig(path, plain); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "mesh:") || strings.Contains(string(data), "rooms:") {
		t.Fatalf("saved config gained room blocks:\n%s", data)
	}
}
