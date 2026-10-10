package node

import (
	"context"
	crand "crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/room"
)

func testRoomConfig() models.RoomConfig {
	cfg := models.DefaultRoomConfig()
	cfg.ID, cfg.Name, cfg.AdminPassword = "lobby", "Lobby", "s3cret-pass"
	cfg.AdvertIntervalMin, cfg.FloodAdvertIntervalHours = 0, 0
	return cfg
}

func TestOpenRoomsCreatesIdentitiesNextToTheDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "data", "kam.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	other := testRoomConfig()
	other.ID, other.Name = "annex", "Annex"

	scheduler := kiss.NewTxScheduler(&kiss.Manager{}, kiss.TxSchedulerConfig{})
	service, repository, err := openRooms(dbPath, models.DefaultMeshConfig(), []models.RoomConfig{testRoomConfig(), other}, scheduler)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if service == nil {
		t.Fatal("no service")
	}
	for _, id := range []string{"lobby", "annex"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "rooms", id+".key")); err != nil {
			t.Fatalf("identity file for %s: %v", id, err)
		}
	}
}

func TestOpenRoomsFailsCleanlyOnACorruptIdentity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kam.db")
	keyDir := room.KeyDir(dbPath)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(room.IdentityPath(keyDir, "lobby"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	scheduler := kiss.NewTxScheduler(&kiss.Manager{}, kiss.TxSchedulerConfig{})
	if service, repository, err := openRooms(dbPath, models.DefaultMeshConfig(), []models.RoomConfig{testRoomConfig()}, scheduler); err == nil {
		_ = repository.Close()
		_ = service
		t.Fatal("a corrupt identity was accepted")
	}
	// The failed attempt released the database; the rest of the node can still use it.
	repository, err := db.OpenRoomRepository(dbPath)
	if err != nil {
		t.Fatalf("database unusable after a failed room start: %v", err)
	}
	_ = repository.Close()
}

// TestRoomLoginThroughTheRadioPipeline drives a login frame through the real
// router, packet tap, room service and transmit scheduler, and reads the reply
// frame the modem would receive.
func TestRoomLoginThroughTheRadioPipeline(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kam.db")
	manager := &kiss.Manager{}
	manager.TestInit()
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{})
	scheduler.Start()
	defer scheduler.Stop()

	cfg := testRoomConfig()
	service, repository, err := openRooms(dbPath, models.DefaultMeshConfig(), []models.RoomConfig{cfg}, scheduler)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service.Start()
	defer service.Stop()
	unsubscribe := manager.SubscribePackets(service.HandlePacket)
	defer unsubscribe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go manager.TestRouteLoop(ctx)

	identities, err := room.LoadIdentities(room.KeyDir(dbPath), []models.RoomConfig{cfg}, nil)
	if err != nil || identities[0].Created {
		t.Fatalf("reload room identity: %v", err)
	}
	roomID := identities[0].Identity
	roomPub := roomID.PublicKey()

	client, err := mesh.GenerateIdentity(crand.Reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := client.SharedSecret(roomPub[:])
	if err != nil {
		t.Fatal(err)
	}
	clientPub := client.PublicKey()
	plain := append([]byte{1, 0, 0, 0, 0, 0, 0, 0}, "s3cret-pass\x00"...) // timestamp 1, sync_since 0, password
	payload, err := mesh.AnonPayload(roomID.Hash(), clientPub[:], secret[:], plain)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mesh.Packet{Header: mesh.MakeHeader(mesh.RouteFlood, mesh.PayloadAnonReq), Payload: payload}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte{kiss.CMD_DATA}, raw...)

	manager.TestRxChan() <- frame
	var reply []byte
	select {
	case reply = <-manager.TestTxChan():
	case <-time.After(3 * time.Second):
		t.Fatal("no reply frame reached the modem")
	}
	if reply[0] != kiss.CMD_DATA {
		t.Fatalf("reply is not a KISS data frame: %x", reply)
	}
	pkt, err := mesh.Parse(reply[1:])
	if err != nil || pkt.PayloadType() != mesh.PayloadPath || !pkt.IsFlood() {
		t.Fatalf("reply packet: %+v, %v", pkt, err)
	}
	body, ok := mesh.MACThenDecrypt(secret[:], pkt.Payload[2:])
	if !ok || pkt.Payload[0] != client.Hash() || pkt.Payload[1] != roomID.Hash() {
		t.Fatal("the reply is not encrypted for this client")
	}
	_, _, extraType, extra, err := mesh.ParsePathReturn(body)
	if err != nil || extraType != mesh.PayloadResponse || extra[4] != 0 || extra[7] != room.RoleAdmin {
		t.Fatalf("login response: type=%d extra=%x err=%v", extraType, extra, err)
	}

	// The same radio frame arriving again (a repeater echo) is a duplicate and gets no second reply.
	manager.TestRxChan() <- frame
	select {
	case again := <-manager.TestTxChan():
		t.Fatalf("a duplicate frame was answered again: %x", again)
	case <-time.After(700 * time.Millisecond):
	}
	if stats := service.Stats(); stats.LoginsOK != 1 {
		t.Fatalf("stats: %+v", stats)
	}
}
