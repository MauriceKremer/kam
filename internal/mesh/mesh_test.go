package mesh

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// MeshCore src/Identity.cpp validatePrivateKey: a known-good client key pair.
var (
	meshCoreTestPrv, _ = hex.DecodeString(
		"7065e18fd9fabb70c1ed90dca19907de698c88b709ea146eafd93d9b830c7b60" +
			"c4681193c79bbc39945ba8064104bb618f8fd7a84a0af6f57033d6e8ddcd6471")
	meshCoreTestPub, _ = hex.DecodeString("1ec77175b0918ed206f9ae04ec136d6d5d4315bb26305427f645b492e9350c10")
)

func seededReader(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func mustIdentity(t *testing.T, seedByte byte) *Identity {
	t.Helper()
	seed := bytes.Repeat([]byte{seedByte}, SeedSize)
	id, err := NewIdentity(seed)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestParseEncodeRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		packet Packet
	}{
		{"flood, no path", Packet{Header: MakeHeader(RouteFlood, PayloadGrpTxt), Payload: []byte{1, 2, 3}}},
		{"flood, two 2-byte hops", Packet{Header: MakeHeader(RouteFlood, PayloadAdvert), PathLen: 0x42, Path: []byte{1, 2, 3, 4}, Payload: []byte{9}}},
		{"scoped flood", Packet{Header: MakeHeader(RouteTransportFlood, PayloadTxtMsg), TransportCodes: [2]uint16{0x1234, 0}, PathLen: 0x01, Path: []byte{7}, Payload: []byte{5, 6}}},
		{"direct, three 3-byte hops", Packet{Header: MakeHeader(RouteDirect, PayloadAck), PathLen: 0x83, Path: bytes.Repeat([]byte{1}, 9), Payload: []byte{1, 2, 3, 4}}},
		{"transport direct", Packet{Header: MakeHeader(RouteTransportDirect, PayloadPath), TransportCodes: [2]uint16{1, 2}, Payload: []byte{1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.packet.Encode()
			if err != nil {
				t.Fatal(err)
			}
			got, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Header != tc.packet.Header || got.TransportCodes != tc.packet.TransportCodes || got.PathLen != tc.packet.PathLen ||
				!bytes.Equal(got.Path, tc.packet.Path) || !bytes.Equal(got.Payload, tc.packet.Payload) {
				t.Fatalf("round trip mismatch: got %+v, want %+v", got, tc.packet)
			}
		})
	}
}

func TestEncodeGoldenFrames(t *testing.T) {
	flood, err := NewFloodPacket(PayloadAdvert, []byte{0xAA, 0xBB}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := flood.Encode()
	if want := []byte{0x11, 0x00, 0xAA, 0xBB}; !bytes.Equal(raw, want) {
		t.Fatalf("flood frame: got %x, want %x", raw, want)
	}

	flood2, _ := NewFloodPacket(PayloadAdvert, []byte{0xAA}, 2, nil)
	raw, _ = flood2.Encode()
	if want := []byte{0x11, 0x40, 0xAA}; !bytes.Equal(raw, want) {
		t.Fatalf("2-byte-hash flood frame: got %x, want %x", raw, want)
	}

	zero := NewZeroHopPacket(PayloadAdvert, []byte{0xAA})
	raw, _ = zero.Encode()
	if want := []byte{0x12, 0x00, 0xAA}; !bytes.Equal(raw, want) {
		t.Fatalf("zero-hop frame: got %x, want %x", raw, want)
	}

	key := RegionKey("#test")
	scoped, _ := NewFloodPacket(PayloadAdvert, []byte{0xAA}, 1, &key)
	raw, _ = scoped.Encode()
	code := key.Code(PayloadAdvert, []byte{0xAA})
	want := []byte{0x10, byte(code), byte(code >> 8), 0, 0, 0x00, 0xAA}
	if !bytes.Equal(raw, want) {
		t.Fatalf("scoped flood frame: got %x, want %x", raw, want)
	}

	direct, _ := NewDirectPacket(PayloadAck, []byte{1, 2, 3, 4}, 0x02, []byte{0xAA, 0xBB})
	raw, _ = direct.Encode()
	if want := []byte{0x0E, 0x02, 0xAA, 0xBB, 1, 2, 3, 4}; !bytes.Equal(raw, want) {
		t.Fatalf("direct frame: got %x, want %x", raw, want)
	}
}

func TestParseRejectsMalformedFrames(t *testing.T) {
	cases := map[string][]byte{
		"empty":                   nil,
		"header only":             {0x11},
		"no payload":              {0x11, 0x00},
		"reserved hash size":      {0x11, 0xC1, 0xAA, 0xBB},
		"path longer than limit":  append([]byte{0x11, 0x40 | 63}, bytes.Repeat([]byte{1}, 126+4)...), // 63 two-byte hops
		"path runs past the end":  {0x11, 0x03, 1, 2},
		"transport codes cut off": {0x10, 1, 2, 3},
		"payload too long":        append([]byte{0x11, 0x00}, bytes.Repeat([]byte{1}, MaxPacketPayload+1)...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse accepted %x", raw)
			}
		})
	}
	if _, err := Parse(append([]byte{0x11, 0x00}, bytes.Repeat([]byte{1}, MaxPacketPayload)...)); err != nil {
		t.Fatalf("payload of exactly MAX_PACKET_PAYLOAD rejected: %v", err)
	}
}

func TestEncodeRejectsInconsistentPackets(t *testing.T) {
	bad := []Packet{
		{Header: MakeHeader(RouteFlood, PayloadAdvert), PathLen: 0x02, Path: []byte{1}, Payload: []byte{1}},
		{Header: MakeHeader(RouteFlood, PayloadAdvert), PathLen: 0xC0, Payload: []byte{1}},
		{Header: MakeHeader(RouteFlood, PayloadAdvert)},
		{Header: MakeHeader(RouteFlood, PayloadAdvert), Payload: make([]byte, MaxPacketPayload+1)},
	}
	for i, p := range bad {
		if _, err := p.Encode(); err == nil {
			t.Fatalf("case %d: Encode accepted %+v", i, p)
		}
	}
	if _, err := EncodePathLen(4, 0); err == nil {
		t.Fatal("hash size 4 accepted")
	}
	if _, err := EncodePathLen(3, 22); err == nil {
		t.Fatal("66-byte path accepted")
	}
}

func TestPacketClassification(t *testing.T) {
	p := Packet{Header: MakeHeader(RouteTransportFlood, PayloadReq), PathLen: 0x42}
	if !p.IsFlood() || p.IsDirect() || !p.HasTransportCodes() || p.HashSize() != 2 || p.HashCount() != 2 {
		t.Fatalf("unexpected classification: %+v", p)
	}
	d := Packet{Header: MakeHeader(RouteDirect, PayloadAck)}
	if d.IsFlood() || !d.IsDirect() || d.HasTransportCodes() || d.PayloadType() != PayloadAck || d.Version() != VersionV1 {
		t.Fatalf("unexpected classification: %+v", d)
	}
}

func TestEnvelopeRoundTripAndTamper(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, SecretSize)
	plain := []byte("hello room server")
	sealed := EncryptThenMAC(secret, plain)
	if len(sealed) != CipherMACSize+32 {
		t.Fatalf("sealed length: got %d, want %d", len(sealed), CipherMACSize+32)
	}
	got, ok := MACThenDecrypt(secret, sealed)
	if !ok || !bytes.Equal(got[:len(plain)], plain) || !bytes.Equal(got[len(plain):], make([]byte, 32-len(plain))) {
		t.Fatalf("decrypt: ok=%v plain=%x", ok, got)
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, ok := MACThenDecrypt(secret, tampered); ok {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, ok := MACThenDecrypt(bytes.Repeat([]byte{0x43}, SecretSize), sealed); ok {
		t.Fatal("wrong secret accepted")
	}
	if _, ok := MACThenDecrypt(secret, sealed[:len(sealed)-1]); ok {
		t.Fatal("ciphertext that is not whole blocks accepted")
	}
	if _, ok := MACThenDecrypt(secret, sealed[:CipherMACSize]); ok {
		t.Fatal("MAC-only input accepted")
	}
	if _, ok := MACThenDecrypt(secret[:16], sealed); ok {
		t.Fatal("short secret accepted")
	}
	if EncryptThenMAC(secret[:16], plain) != nil {
		t.Fatal("short secret accepted for encryption")
	}
}

func TestEnvelopeMatchesIndependentConstruction(t *testing.T) {
	secret := make([]byte, SecretSize)
	for i := range secret {
		secret[i] = byte(i)
	}
	sealed := EncryptThenMAC(secret, bytes.Repeat([]byte{0xAB}, 16))
	mac := hmac.New(sha256.New, secret)
	mac.Write(sealed[CipherMACSize:])
	if !bytes.Equal(mac.Sum(nil)[:CipherMACSize], sealed[:CipherMACSize]) {
		t.Fatal("MAC is not HMAC-SHA-256(secret, ciphertext)[0:2]")
	}
}

func TestEdwardsToMontgomeryMatchesDerivedPublicKeys(t *testing.T) {
	// X25519(scalar, basepoint) is computed by the standard library, so it is
	// independent of the Edwards-to-Montgomery formula under test.
	for seedByte := byte(1); seedByte < 20; seedByte++ {
		id := mustIdentity(t, seedByte)
		pub := id.PublicKey()
		u, err := edwardsToMontgomery(pub[:])
		if err != nil {
			t.Fatal(err)
		}
		if want := id.ecdh.PublicKey().Bytes(); !bytes.Equal(u[:], want) {
			t.Fatalf("seed %d: u = %x, want %x", seedByte, u, want)
		}
	}
}

func TestSharedSecretMatchesMeshCoreTestKeyPair(t *testing.T) {
	if len(meshCoreTestPrv) != 64 || len(meshCoreTestPub) != 32 {
		t.Fatalf("test vector lengths: %d, %d", len(meshCoreTestPrv), len(meshCoreTestPub))
	}
	// MeshCore stores the clamped scalar in the first 32 bytes of the private key.
	theirs, err := ecdh.X25519().NewPrivateKey(meshCoreTestPrv[:32])
	if err != nil {
		t.Fatal(err)
	}
	u, err := edwardsToMontgomery(meshCoreTestPub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u[:], theirs.PublicKey().Bytes()) {
		t.Fatalf("test public key does not convert to the X25519 key of the test private key: %x vs %x", u, theirs.PublicKey().Bytes())
	}

	for seedByte := byte(1); seedByte < 10; seedByte++ {
		ours := mustIdentity(t, seedByte)
		ourPub := ours.PublicKey()
		ourU, err := edwardsToMontgomery(ourPub[:])
		if err != nil {
			t.Fatal(err)
		}
		remote, err := ecdh.X25519().NewPublicKey(ourU[:])
		if err != nil {
			t.Fatal(err)
		}
		want, err := theirs.ECDH(remote) // what the MeshCore side computes
		if err != nil {
			t.Fatal(err)
		}
		got, err := ours.SharedSecret(meshCoreTestPub)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:], want) {
			t.Fatalf("seed %d: shared secret %x, MeshCore side computes %x", seedByte, got, want)
		}
	}
}

func TestSharedSecretIsSymmetric(t *testing.T) {
	random := seededReader(1)
	a, err := GenerateIdentity(random, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateIdentity(random, nil)
	if err != nil {
		t.Fatal(err)
	}
	pubA, pubB := a.PublicKey(), b.PublicKey()
	ab, err := a.SharedSecret(pubB[:])
	if err != nil {
		t.Fatal(err)
	}
	ba, err := b.SharedSecret(pubA[:])
	if err != nil {
		t.Fatal(err)
	}
	if ab != ba || ab == ([SecretSize]byte{}) {
		t.Fatalf("shared secrets differ or are zero: %x %x", ab, ba)
	}
}

func TestSharedSecretRejectsBadPeers(t *testing.T) {
	id := mustIdentity(t, 5)
	if _, err := id.SharedSecret(make([]byte, 31)); err == nil {
		t.Fatal("31-byte key accepted")
	}
	// y = 1 is the Edwards identity: 1-y is zero, so there is no Montgomery image.
	identity := make([]byte, 32)
	identity[0] = 1
	if _, err := id.SharedSecret(identity); err == nil {
		t.Fatal("identity point accepted")
	}
	// y = -1 maps to u = 0, a low-order point; X25519 must refuse it.
	order2 := bytes.Repeat([]byte{0xFF}, 32)
	order2[0] = 0xEC
	order2[31] = 0x7F
	if _, err := id.SharedSecret(order2); err == nil {
		t.Fatal("low-order point accepted")
	}
}

func TestGenerateIdentityRejectsReservedAndVetoedHashes(t *testing.T) {
	random := seededReader(7)
	vetoed := map[byte]bool{}
	for i := 0; i < 200; i++ {
		id, err := GenerateIdentity(random, func(h byte) bool { return vetoed[h] })
		if err != nil {
			t.Fatal(err)
		}
		if h := id.Hash(); h == 0x00 || h == 0xFF || vetoed[h] {
			t.Fatalf("accepted hash %#02x", h)
		}
		vetoed[id.Hash()] = true
	}
	if _, err := GenerateIdentity(random, func(byte) bool { return true }); !errors.Is(err, ErrNoFreeIdent) {
		t.Fatalf("always-reject: got %v, want ErrNoFreeIdent", err)
	}
	if _, err := GenerateIdentity(nil, nil); err == nil {
		t.Fatal("nil random source accepted")
	}
}

func TestIdentitySigns(t *testing.T) {
	id := mustIdentity(t, 9)
	pub := id.PublicKey()
	sig := id.Sign([]byte("message"))
	if !ed25519.Verify(pub[:], []byte("message"), sig) {
		t.Fatal("signature does not verify")
	}
	if _, err := NewIdentity(make([]byte, 31)); !errors.Is(err, ErrBadSeedSize) {
		t.Fatalf("short seed: got %v", err)
	}
}

func TestLoadOrCreateIdentityPersistsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms", "lobby.key")
	first, created, err := LoadOrCreateIdentity(path, seededReader(3), nil)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	second, created, err := LoadOrCreateIdentity(path, seededReader(99), nil)
	if err != nil || created {
		t.Fatalf("reload: created=%v err=%v", created, err)
	}
	if first.PublicKey() != second.PublicKey() {
		t.Fatal("reloaded identity differs")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("key file mode: got %o, want 600", mode)
		}
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if mode := dir.Mode().Perm(); mode != 0o700 {
			t.Fatalf("key directory mode: got %o, want 700", mode)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestLoadOrCreateIdentityRefusesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.key")
	for _, content := range []string{"", "not hex\n", strings.Repeat("ab", 31)} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreateIdentity(path, seededReader(1), nil); err == nil {
			t.Fatalf("corrupt key file %q accepted; a new key would change the room address", content)
		}
		if data, _ := os.ReadFile(path); string(data) != content {
			t.Fatal("corrupt key file was overwritten")
		}
	}
}

func TestAdvertAppDataLayoutAndLimits(t *testing.T) {
	plain, err := AdvertInfo{Type: AdvertTypeRoom, Name: "Lobby"}.AppData()
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{0x83}, "Lobby"...); !bytes.Equal(plain, want) {
		t.Fatalf("appdata: got %x, want %x", plain, want)
	}

	located, err := AdvertInfo{Type: AdvertTypeRoom, Name: "Lobby", HasLocation: true, Latitude: 52.5, Longitude: -1.25}.AppData()
	if err != nil {
		t.Fatal(err)
	}
	if located[0] != 0x93 || int32(binary.LittleEndian.Uint32(located[1:5])) != 52_500_000 || int32(binary.LittleEndian.Uint32(located[5:9])) != -1_250_000 || string(located[9:]) != "Lobby" {
		t.Fatalf("located appdata: %x", located)
	}

	if _, err := (AdvertInfo{Type: AdvertTypeRoom, Name: strings.Repeat("a", 31)}).AppData(); err != nil {
		t.Fatalf("31-byte name rejected: %v", err)
	}
	if _, err := (AdvertInfo{Type: AdvertTypeRoom, Name: strings.Repeat("a", 32)}).AppData(); err == nil {
		t.Fatal("32-byte name accepted")
	}
	if _, err := (AdvertInfo{Type: AdvertTypeRoom, Name: strings.Repeat("a", 23), HasLocation: true}).AppData(); err != nil {
		t.Fatalf("23-byte name with location rejected: %v", err)
	}
	if _, err := (AdvertInfo{Type: AdvertTypeRoom, Name: strings.Repeat("a", 24), HasLocation: true}).AppData(); err == nil {
		t.Fatal("24-byte name with location accepted")
	}
	if _, err := (AdvertInfo{Type: AdvertTypeRoom}).AppData(); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestAdvertPayloadIsSignedOverPubkeyTimestampAppData(t *testing.T) {
	id := mustIdentity(t, 11)
	appData, _ := AdvertInfo{Type: AdvertTypeRoom, Name: "Lobby"}.AppData()
	payload, err := AdvertPayload(id, 1_700_000_000, appData)
	if err != nil {
		t.Fatal(err)
	}
	pub := id.PublicKey()
	if !bytes.Equal(payload[:32], pub[:]) || binary.LittleEndian.Uint32(payload[32:36]) != 1_700_000_000 || !bytes.Equal(payload[100:], appData) {
		t.Fatalf("advert layout wrong: %x", payload)
	}
	signed := append(append([]byte(nil), payload[:36]...), appData...)
	if !ed25519.Verify(pub[:], signed, payload[36:100]) {
		t.Fatal("advert signature does not verify")
	}
	if _, err := AdvertPayload(id, 1, make([]byte, MaxAdvertDataSize+1)); err == nil {
		t.Fatal("oversized appdata accepted")
	}
}

func TestRegionKeyAndCode(t *testing.T) {
	sum := sha256.Sum256([]byte("#europe"))
	want := TransportKey(sum[:16])
	for _, name := range []string{"#europe", "europe", " europe "} {
		if RegionKey(name) != want {
			t.Fatalf("RegionKey(%q) differs from SHA-256(\"#europe\")[:16]", name)
		}
	}
	if RegionKey("##europe") == want {
		t.Fatal("only one leading # may be stripped")
	}

	payload := []byte{1, 2, 3, 4}
	mac := hmac.New(sha256.New, want[:])
	mac.Write([]byte{PayloadGrpTxt})
	mac.Write(payload)
	sum2 := mac.Sum(nil)
	expected := binary.LittleEndian.Uint16(sum2)
	if expected == 0 || expected == 0xFFFF {
		t.Fatalf("test vector hit a reserved code %#04x; pick another payload", expected)
	}
	if got := want.Code(PayloadGrpTxt, payload); got != expected {
		t.Fatalf("code: got %#04x, want %#04x", got, expected)
	}
	if want.Code(PayloadAdvert, payload) == expected {
		t.Fatal("code does not depend on the payload type")
	}
	if want.Code(PayloadGrpTxt, []byte{9}) == expected {
		t.Fatal("code does not depend on the payload")
	}
	if !(TransportKey{}).IsZero() || want.IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestPathReturnRoundTrip(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, SecretSize)
	path := []byte{0xAA, 0xBB}
	payload, err := PathReturnPayload(0x11, 0x22, secret, 0x02, path, PayloadResponse, []byte("thirteen byte"), seededReader(1))
	if err != nil {
		t.Fatal(err)
	}
	if payload[0] != 0x11 || payload[1] != 0x22 {
		t.Fatalf("hashes: %x", payload[:2])
	}
	plain, ok := MACThenDecrypt(secret, payload[2:])
	if !ok {
		t.Fatal("MAC failed")
	}
	pathLen, gotPath, extraType, extra, err := ParsePathReturn(plain)
	if err != nil || pathLen != 0x02 || !bytes.Equal(gotPath, path) || extraType != PayloadResponse || !bytes.HasPrefix(extra, []byte("thirteen byte")) {
		t.Fatalf("parse: %v %x %x %x %q", err, pathLen, gotPath, extraType, extra)
	}

	bare, err := PathReturnPayload(0x11, 0x22, secret, 0x00, nil, 0, nil, seededReader(1))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ = MACThenDecrypt(secret, bare[2:])
	if _, _, extraType, extra, err = ParsePathReturn(plain); err != nil || extraType != 0x0F || len(extra) < 4 {
		t.Fatalf("bare path return: type=%#x extra=%x err=%v", extraType, extra, err)
	}

	long := make([]byte, 60)
	if _, err := PathReturnPayload(1, 2, secret, 60, long, PayloadResponse, make([]byte, 110), seededReader(1)); !errors.Is(err, ErrPayloadTooLong) {
		t.Fatalf("oversized path return: got %v", err)
	}
	if _, err := PathReturnPayload(1, 2, secret, 0xC1, []byte{1, 2, 3, 4}, 0, nil, seededReader(1)); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("reserved hash size: got %v", err)
	}
	if _, _, _, _, err := ParsePathReturn([]byte{0x02, 1}); err == nil {
		t.Fatal("truncated path accepted")
	}
	if _, _, _, _, err := ParsePathReturn(nil); err == nil {
		t.Fatal("empty path return accepted")
	}
}

func TestPeerAndAnonPayloadLimits(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, SecretSize)
	payload, err := PeerPayload(1, 2, secret, make([]byte, 176))
	if err != nil || len(payload) != 2+2+176 {
		t.Fatalf("176-byte plaintext: len=%d err=%v", len(payload), err)
	}
	if _, err := PeerPayload(1, 2, secret, make([]byte, 177)); !errors.Is(err, ErrPayloadTooLong) {
		t.Fatalf("177-byte plaintext: got %v", err)
	}
	if _, err := PeerPayload(1, 2, secret[:16], []byte{1}); !errors.Is(err, ErrBadSecret) {
		t.Fatalf("short secret: got %v", err)
	}
	pub := mustIdentity(t, 3).PublicKey()
	anon, err := AnonPayload(9, pub[:], secret, []byte("login"))
	if err != nil || anon[0] != 9 || !bytes.Equal(anon[1:33], pub[:]) {
		t.Fatalf("anon payload: %x err=%v", anon, err)
	}
	if _, err := AnonPayload(9, pub[:5], secret, []byte("x")); !errors.Is(err, ErrBadKey) {
		t.Fatalf("short pubkey: got %v", err)
	}
}

func TestAckHashAndTruncation(t *testing.T) {
	pub := bytes.Repeat([]byte{3}, 32)
	want := sha256.Sum256(append([]byte("data"), pub...))
	if got := AckHash([]byte("data"), pub); !bytes.Equal(got[:], want[:4]) {
		t.Fatalf("ack hash: got %x, want %x", got, want[:4])
	}

	if got := TruncateUTF8("hello", 10); got != "hello" {
		t.Fatalf("short string changed: %q", got)
	}
	if got := TruncateUTF8("héllo", 2); got != "h" {
		t.Fatalf("cut inside a rune: got %q, want %q", got, "h")
	}
	if got := TruncateUTF8("héllo", 3); got != "hé" {
		t.Fatalf("cut on a rune boundary: got %q", got)
	}
	if got := TruncateUTF8("日本語", 4); got != "日" {
		t.Fatalf("multi-byte: got %q", got)
	}
	if got := TruncateUTF8("日本語", 0); got != "" {
		t.Fatalf("zero length: got %q", got)
	}
}
