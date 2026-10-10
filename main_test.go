package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/mesh"
)

func TestRunChannelCommandUpsertsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.yml")
	content := `version: 1
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := runChannelCommand([]string{
		"upsert",
		"-config", path,
		"-name", "#cli-test",
		"-key-strategy", "derived",
	}); err != nil {
		t.Fatalf("run channel command: %v", err)
	}

	cfg, err := config.LoadKamConfig(path)
	if err != nil {
		t.Fatalf("load updated config: %v", err)
	}
	index := cfg.ChannelIndex("#cli-test")
	if index < 0 || !cfg.Channels[index].Enabled {
		t.Fatalf("CLI did not persist enabled channel: %+v", cfg.Channels)
	}
}

const roomShowConfig = `version: 1
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
mesh:
  default_scope: "#eu"
rooms:
  - id: lobby
    name: "KAM Lobby"
    admin_password: "s3cret-pass"
  - id: broken
    name: "Broken"
    admin_password: "password"
`

func TestRunRoomShowPrintsKeyAdvertAndContactURI(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "kam.yml")
	if err := os.WriteFile(configPath, []byte(roomShowConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"show", "-config", configPath, "-db", filepath.Join(dir, "kam.db")}

	var out, errOut bytes.Buffer
	if err := runRoomCommand(args, &out, &errOut); err != nil {
		t.Fatalf("room show: %v", err)
	}
	if !strings.Contains(errOut.String(), "room broken disabled") {
		t.Fatalf("an invalid room must be reported, got stderr %q", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "s3cret-pass") {
		t.Fatal("the admin password was printed")
	}
	if !strings.Contains(out.String(), "created now") {
		t.Fatalf("first run must create the key:\n%s", out.String())
	}
	keyFile := filepath.Join(dir, "rooms", "lobby.key")
	info, err := os.Stat(keyFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	seedHex, _ := os.ReadFile(keyFile)
	if strings.Contains(out.String(), strings.TrimSpace(string(seedHex))) {
		t.Fatal("the private seed was printed")
	}

	field := func(label string) string {
		m := regexp.MustCompile(`(?m)^  ` + label + `\s*: (.+)$`).FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("no %q line in:\n%s", label, out.String())
		}
		return strings.Fields(m[1])[0]
	}
	pubHex := field("public key")
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != 32 {
		t.Fatalf("public key %q", pubHex)
	}
	if want := "meshcore://contact/add?name=KAM+Lobby&public_key=" + pubHex + "&type=3"; field("contact URI") != want {
		t.Fatalf("contact URI %q, want %q", field("contact URI"), want)
	}

	// The advert is a valid signed flood packet in the default scope.
	raw, err := hex.DecodeString(field("advert hex"))
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := mesh.Parse(raw)
	if err != nil || pkt.PayloadType() != mesh.PayloadAdvert || pkt.RouteType() != mesh.RouteTransportFlood {
		t.Fatalf("advert packet: %+v, %v", pkt, err)
	}
	scope := mesh.RegionKey("#eu")
	if pkt.TransportCodes[0] != scope.Code(mesh.PayloadAdvert, pkt.Payload) {
		t.Fatal("advert is not scoped to the default region")
	}
	if !bytes.Equal(pkt.Payload[:32], pub) {
		t.Fatal("advert is signed by a different key")
	}
	signed := append(append([]byte(nil), pkt.Payload[:36]...), pkt.Payload[100:]...)
	if !ed25519.Verify(pub, signed, pkt.Payload[36:100]) {
		t.Fatal("advert signature does not verify")
	}
	if string(pkt.Payload[101:]) != "KAM Lobby" || pkt.Payload[100] != 0x83 {
		t.Fatalf("advert appdata %x", pkt.Payload[100:])
	}

	// A second run shows the same key and does not create a new one.
	var again bytes.Buffer
	if err := runRoomCommand(args, &again, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), pubHex) || !strings.Contains(again.String(), "existing") {
		t.Fatalf("second run changed the identity:\n%s", again.String())
	}
}

func TestRunRoomCommandErrors(t *testing.T) {
	dir := t.TempDir()
	noRooms := filepath.Join(dir, "none.yml")
	if err := os.WriteFile(noRooms, []byte(strings.Split(roomShowConfig, "mesh:")[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"no subcommand":      nil,
		"unknown subcommand": {"list"},
		"missing config":     {"show", "-config", filepath.Join(dir, "missing.yml")},
		"no rooms":           {"show", "-config", noRooms},
		"extra arguments":    {"show", "-config", noRooms, "surplus"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := runRoomCommand(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
