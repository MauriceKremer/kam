package parsers

import (
	"bytes"
	"crypto/aes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"kam.bytecoder.nl/internal/mesh"
)

// PublicChannelSecret is the 32-byte GroupChannel secret for the public channel.
// First 16 bytes are the literal key "MeshCore_Public_"; remaining 16 are zero.
var PublicChannelSecret = func() []byte {
	s := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(s, psk)
	return s
}()

// SetPublicChannelSecretHex overrides the default public channel secret.
func SetPublicChannelSecretHex(keyHex string) error {
	psk, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return fmt.Errorf("decode public key_hex: %w", err)
	}
	if len(psk) != 16 {
		return fmt.Errorf("public key_hex must decode to 16 bytes, got %d", len(psk))
	}

	secret := make([]byte, 32)
	copy(secret, psk)
	registryMutex.Lock()
	PublicChannelSecret = secret
	registryMutex.Unlock()
	return nil
}

// GroupTextMessage represents a decrypted mesh chat message.
type GroupTextMessage struct {
	ChannelName      string
	Timestamp        time.Time
	Message          string
	Sender           string
	Body             string
	TxtType          uint8
	Attempt          uint8
	PublicKeyPrefix  []byte
	TimeDriftSeconds int64

	// HopCount is the number of relay hops recorded in the inbound packet's
	// path field. Populated by the router, not the parser, since it is a
	// network-layer attribute outside the encrypted payload.
	HopCount int

	// HopStations is the list of per-hop path hashes (raw bytes, size per the
	// packet's path hash size code). Populated by the router.
	HopStations [][]byte

	// HasRxMeta reports whether the modem emitted an RxMeta (0xF9) frame for
	// this packet. When false, SNR/RSSI are zero and should be treated as
	// absent rather than measured-zero.
	HasRxMeta bool

	// SNR is the raw RxMeta SNR value (signed, quarter-dB units: divide by 4
	// for dB). Populated by the router from the modem's RxMeta frame.
	SNR int

	// RSSI is the RxMeta RSSI in dBm (signed). Populated by the router.
	RSSI int
}

// PublicChannelHash returns the channel hash for the currently configured public secret.
func PublicChannelHash() byte {
	registryMutex.RLock()
	defer registryMutex.RUnlock()
	return ChannelHash(PublicChannelSecret)
}

// topicStorage holds cryptographic parameters derived from a topic string name.
type topicStorage struct {
	name   string
	secret []byte // 32-byte secret
	hash   byte
}

var (
	registryMutex sync.RWMutex
	topicRegistry = make(map[uint32]topicStorage)
)

type TopicConfig struct {
	Name        string
	KeyStrategy string
	KeyHex      string
	Enabled     bool
}

// ReplaceTopicRegistry validates and atomically replaces all configured channel secrets.
func ReplaceTopicRegistry(publicKeyHex string, channels []TopicConfig) error {
	publicKey, err := hex.DecodeString(strings.TrimSpace(publicKeyHex))
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if len(publicKey) != 16 {
		return fmt.Errorf("public key must decode to 16 bytes, got %d", len(publicKey))
	}

	publicSecret := make([]byte, 32)
	copy(publicSecret, publicKey)
	topics := make(map[uint32]topicStorage, len(channels))
	for _, channel := range channels {
		cleanName := normalizeTopicName(channel.Name)
		if cleanName == "#public" || !channel.Enabled {
			continue
		}

		var secret []byte
		switch channel.KeyStrategy {
		case "derived":
			hash := sha256.Sum256([]byte(cleanName))
			secret = make([]byte, 32)
			copy(secret, hash[:16])
		case "explicit":
			key, err := hex.DecodeString(strings.TrimSpace(channel.KeyHex))
			if err != nil {
				return fmt.Errorf("decode key for channel %q: %w", cleanName, err)
			}
			if len(key) != 16 && len(key) != 32 {
				return fmt.Errorf("channel %q key must decode to 16 or 32 bytes, got %d", cleanName, len(key))
			}
			secret = make([]byte, 32)
			copy(secret, key)
		default:
			return fmt.Errorf("unsupported key strategy %q for channel %q", channel.KeyStrategy, cleanName)
		}

		nameHash := sha256.Sum256([]byte(cleanName))
		topicID := binary.LittleEndian.Uint32(nameHash[28:32])
		topics[topicID] = topicStorage{
			name:   cleanName,
			secret: secret,
			hash:   ChannelHash(secret),
		}
	}

	registryMutex.Lock()
	PublicChannelSecret = publicSecret
	topicRegistry = topics
	registryMutex.Unlock()
	return nil
}

// ResetTopicRegistry clears all registered topic channels.
func ResetTopicRegistry() {
	registryMutex.Lock()
	topicRegistry = make(map[uint32]topicStorage)
	registryMutex.Unlock()
}

// RegisterTopic derives and registers a channel secret from the topic name.
func RegisterTopic(topicName string) uint32 {
	cleanName := normalizeTopicName(topicName)

	if cleanName == "#" || cleanName == "#public" {
		return 0
	}

	hash := sha256.Sum256([]byte(cleanName))
	secret := make([]byte, 32)
	copy(secret, hash[:16])
	topicID := binary.LittleEndian.Uint32(hash[28:32])

	registryMutex.Lock()
	topicRegistry[topicID] = topicStorage{
		name:   cleanName,
		secret: secret,
		hash:   ChannelHash(secret),
	}
	registryMutex.Unlock()
	slog.Info("[Topic Registry] Subscribed to new channel", "topic", cleanName, "id", topicID, "key_hex", hex.EncodeToString(secret[:16]))
	return topicID
}

// RegisterTopicWithKeyHex registers a topic with an explicit 16 or 32 byte key.
func RegisterTopicWithKeyHex(topicName, keyHex string) (uint32, error) {
	cleanName := normalizeTopicName(topicName)
	if cleanName == "#" || cleanName == "#public" {
		return 0, fmt.Errorf("explicit key strategy is not valid for %q", cleanName)
	}

	keyBytes, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return 0, fmt.Errorf("decode explicit channel key: %w", err)
	}
	if len(keyBytes) != 16 && len(keyBytes) != 32 {
		return 0, fmt.Errorf("explicit channel key must decode to 16 or 32 bytes, got %d", len(keyBytes))
	}

	hash := sha256.Sum256([]byte(cleanName))
	topicID := binary.LittleEndian.Uint32(hash[28:32])

	secret := make([]byte, 32)
	copy(secret, keyBytes)

	registryMutex.Lock()
	topicRegistry[topicID] = topicStorage{
		name:   cleanName,
		secret: secret,
		hash:   ChannelHash(secret),
	}
	registryMutex.Unlock()

	slog.Info("[Topic Registry] Subscribed to explicit-key channel", "topic", cleanName, "id", topicID, "key_hex", hex.EncodeToString(secret[:16]))
	return topicID, nil
}

// TopicRegistered reports whether a non-public channel is currently
// registered in the in-memory topic registry. Intended for tests.
func TopicRegistered(topicName string) bool {
	cleanName := normalizeTopicName(topicName)
	if cleanName == "#" || cleanName == "#public" {
		return false
	}
	hash := sha256.Sum256([]byte(cleanName))
	topicID := binary.LittleEndian.Uint32(hash[28:32])
	registryMutex.RLock()
	_, ok := topicRegistry[topicID]
	registryMutex.RUnlock()
	return ok
}

// UnregisterTopic removes a topic from the registry.
func UnregisterTopic(topicName string) {
	cleanName := normalizeTopicName(topicName)
	if cleanName == "#" || cleanName == "#public" {
		return
	}

	hash := sha256.Sum256([]byte(cleanName))
	topicID := binary.LittleEndian.Uint32(hash[28:32])

	registryMutex.Lock()
	delete(topicRegistry, topicID)
	registryMutex.Unlock()

	slog.Info("[Topic Registry] Unsubscribed channel", "topic", cleanName, "id", topicID)
}

func normalizeTopicName(topicName string) string {
	cleanName := strings.ToLower(strings.TrimSpace(topicName))
	if !strings.HasPrefix(cleanName, "#") {
		cleanName = "#" + cleanName
	}
	return cleanName
}

// LookupTopic searches the registry and public channel by normalized name.
// Returns secret, channel hash, channel name, and ok.
func LookupTopic(topicName string) (secret []byte, hash byte, name string, ok bool) {
	cleanName := normalizeTopicName(topicName)
	if cleanName == "#public" {
		registryMutex.RLock()
		secret := append([]byte(nil), PublicChannelSecret...)
		registryMutex.RUnlock()
		return secret, ChannelHash(secret), "#public", true
	}

	want := sha256.Sum256([]byte(cleanName))
	topicID := binary.LittleEndian.Uint32(want[28:32])

	registryMutex.RLock()
	topic, exists := topicRegistry[topicID]
	secret = append([]byte(nil), topic.secret...)
	registryMutex.RUnlock()

	if !exists {
		return nil, 0, "", false
	}

	return secret, topic.hash, topic.name, true
}

// ChannelHash returns the first byte of SHA256(secret).
// If the secret's upper 16 bytes are all zero, it hashes only the lower 16 bytes
// to match BaseChatMesh::setChannel behavior for 16-byte keys.
func ChannelHash(secret32 []byte) byte {
	if len(secret32) != 32 {
		return 0
	}

	isUpperZero := true
	for _, b := range secret32[16:] {
		if b != 0 {
			isUpperZero = false
			break
		}
	}

	if isUpperZero {
		sum := sha256.Sum256(secret32[:16])
		return sum[0]
	}
	sum := sha256.Sum256(secret32)
	return sum[0]
}

// AESECBEncrypt encrypts src block-by-block using AES-128-ECB with the given
// 16-byte key. src must be a multiple of 16 bytes.
func AESECBEncrypt(key, src []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += 16 {
		block.Encrypt(dst[i:], src[i:])
	}
	return dst
}

// EncryptThenMAC encrypts plaintext with AES-128-ECB and prepends a 2-byte
// HMAC-SHA256 over the ciphertext. Key material uses the first 16 bytes of
// secret32 for AES and the full 32 bytes for HMAC. plaintext must already be a
// multiple of 16 bytes; mesh.EncryptThenMAC pads instead.
func EncryptThenMAC(secret32, plaintext []byte) []byte {
	if len(plaintext)%16 != 0 {
		return nil
	}
	return mesh.EncryptThenMAC(secret32, plaintext)
}

// AESECBDecrypt decrypts src block-by-block using AES-128-ECB with the given
// 16-byte key. src must be a multiple of 16 bytes.
func AESECBDecrypt(key, src []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += 16 {
		block.Decrypt(dst[i:], src[i:])
	}
	return dst, nil
}

// macThenDecrypt verifies the 2-byte HMAC-SHA256 prefix and decrypts the rest
// with AES-128-ECB. src format: [2-byte HMAC] [AES-128-ECB ciphertext].
// secret32 is the 32-byte channel secret: first 16 bytes are the AES key,
// all 32 bytes are the HMAC key (matching MeshCore Utils::MACThenDecrypt).
// Returns decrypted plaintext, or nil if MAC verification fails.
func macThenDecrypt(secret32, src []byte) []byte {
	pt, ok := mesh.MACThenDecrypt(secret32, src)
	if !ok {
		return nil
	}
	return pt
}

// ParseGroupText decrypts and decodes a MeshCore group text payload.
func ParseGroupText(payload []byte) (GroupTextMessage, bool) {
	slog.Debug("[Group Text] Received packet", "payload_len", len(payload))

	// Minimum: 1 (channel_hash) + 2 (MAC) + 16 (one AES block) = 19 bytes
	if len(payload) < 19 {
		slog.Warn("[Group Text] Dropping packet: payload too short")
		return GroupTextMessage{}, false
	}

	macAndData := payload[1:] // [2-byte HMAC] + [AES-128-ECB ciphertext]

	var decryptedPlaintext []byte
	var matchedChannelName string

	// Try public channel
	registryMutex.RLock()
	publicSecret := append([]byte(nil), PublicChannelSecret...)
	if pt := macThenDecrypt(publicSecret, macAndData); pt != nil {
		decryptedPlaintext = pt
		matchedChannelName = "#public"
	}

	// Try registered topic channels
	if decryptedPlaintext == nil {
		for _, topic := range topicRegistry {
			if pt := macThenDecrypt(topic.secret, macAndData); pt != nil {
				decryptedPlaintext = pt
				matchedChannelName = topic.name
				break
			}
		}
	}
	registryMutex.RUnlock()

	slog.Debug("[Group Text] Decryption attempt finished", "channel_matched", matchedChannelName != "")

	if len(decryptedPlaintext) < 5 {
		slog.Warn("[Group Text] Dropping packet: decryption failed or plaintext too short")
		return GroupTextMessage{}, false
	}

	rawTimestamp := binary.LittleEndian.Uint32(decryptedPlaintext[0:4])
	txTime := time.Unix(int64(rawTimestamp), 0)

	timeWindow := 30 * time.Minute
	if time.Since(txTime) > timeWindow || time.Until(txTime) > timeWindow {
		slog.Warn("[Group Text] Dropping message: failed anti-replay freshness check")
		return GroupTextMessage{}, false
	}

	flagsByte := decryptedPlaintext[4]
	txtType := (flagsByte >> 2) & 0x3F
	attempt := flagsByte & 0x03

	// Strip null padding added by AES block alignment
	// Skip 4-byte timestamp + 1-byte flags (plaintext format per MeshCore spec)
	msgBytes := decryptedPlaintext[5:]
	if i := bytes.IndexByte(msgBytes, 0); i >= 0 {
		msgBytes = msgBytes[:i]
	}

	var pubKeyPrefix []byte
	if txtType == 0x02 && len(msgBytes) >= 4 {
		pubKeyPrefix = msgBytes[:4]
		msgBytes = msgBytes[4:]
	}

	message := string(msgBytes)
	sender, body := splitGroupTextMessage(message)

	successMsgPacket := GroupTextMessage{
		ChannelName:      matchedChannelName,
		Timestamp:        txTime,
		Message:          message,
		Sender:           sender,
		Body:             body,
		TxtType:          txtType,
		Attempt:          attempt,
		PublicKeyPrefix:  pubKeyPrefix,
		TimeDriftSeconds: int64(time.Since(txTime).Abs().Seconds()),
	}

	slog.Debug("[Group Text] Broadcast Message Received",
		"channel", successMsgPacket.ChannelName,
		"time", successMsgPacket.Timestamp.Format("15:04:05"),
		"txt_type", successMsgPacket.TxtType,
		"attempt", successMsgPacket.Attempt,
		"sender", successMsgPacket.Sender,
		"body", successMsgPacket.Body,
		"pubkey_prefix", hex.EncodeToString(successMsgPacket.PublicKeyPrefix),
	)

	return successMsgPacket, true
}

func splitGroupTextMessage(message string) (sender, body string) {
	if i := strings.Index(message, ": "); i >= 0 {
		return message[:i], message[i+2:]
	}
	return "", message
}
