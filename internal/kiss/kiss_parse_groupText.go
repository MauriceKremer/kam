package kiss

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// PublicChannelSecret is the 32-byte GroupChannel secret for the public channel.
// First 16 bytes are the literal key "MeshCore_Public_"; remaining 16 are zero.
var PublicChannelSecret = func() []byte {
	s := make([]byte, 32)
	psk, _ := hex.DecodeString("8b3387e9c5cdea6ac9e5edbaa115cd72")
	copy(s, psk)
	return s
}()

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
	PublicChannelSecret = secret
	return nil
}

// GroupTextMessage represents a decrypted mesh chat message
type GroupTextMessage struct {
	ChannelName string
	Timestamp   time.Time
	Message     string
}

// topicStorage holds cryptographic parameters derived from a topic string name
type topicStorage struct {
	name   string
	secret []byte // 32-byte secret: full SHA256(channel_name)
}

var (
	registryMutex sync.RWMutex
	topicRegistry = make(map[uint32]topicStorage)
)

func ResetTopicRegistry() {
	registryMutex.Lock()
	topicRegistry = make(map[uint32]topicStorage)
	registryMutex.Unlock()
}

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
	}
	registryMutex.Unlock()

	slog.Info("[Topic Registry] Subscribed to new channel", "topic", cleanName, "id", topicID, "key_hex", hex.EncodeToString(secret[:16]))
	return topicID
}

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
	}
	registryMutex.Unlock()

	slog.Info("[Topic Registry] Subscribed to explicit-key channel", "topic", cleanName, "id", topicID, "key_hex", hex.EncodeToString(secret[:16]))
	return topicID, nil
}

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

// aesECBDecrypt decrypts src block-by-block using AES-128-ECB with the given
// 16-byte key. src must be a multiple of 16 bytes.
func aesECBDecrypt(key, src []byte) ([]byte, error) {
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
	const macSize = 2
	if len(src) < macSize+16 || len(src[macSize:])%16 != 0 {
		return nil
	}
	ciphertext := src[macSize:]

	h := hmac.New(sha256.New, secret32)
	h.Write(ciphertext)
	if !hmac.Equal(h.Sum(nil)[:macSize], src[:macSize]) {
		return nil
	}

	pt, err := aesECBDecrypt(secret32[:16], ciphertext)
	if err != nil {
		return nil
	}
	return pt
}

func (m *Manager) handleGroupText(payload []byte) {
	slog.Info("[Group Text] Received packet", "payload_len", len(payload))

	// Minimum: 1 (channel_hash) + 2 (MAC) + 16 (one AES block) = 19 bytes
	if len(payload) < 19 {
		slog.Warn("[Group Text] Dropping packet: payload too short")
		return
	}

	macAndData := payload[1:] // [2-byte HMAC] + [AES-128-ECB ciphertext]

	var decryptedPlaintext []byte
	var matchedChannelName string

	// Try public channel
	if pt := macThenDecrypt(PublicChannelSecret, macAndData); pt != nil {
		decryptedPlaintext = pt
		matchedChannelName = "#public"
	}

	// Try registered topic channels
	if decryptedPlaintext == nil {
		registryMutex.RLock()
		for _, topic := range topicRegistry {
			if pt := macThenDecrypt(topic.secret, macAndData); pt != nil {
				decryptedPlaintext = pt
				matchedChannelName = topic.name
				break
			}
		}
		registryMutex.RUnlock()
	}

	slog.Info("[Group Text] Decryption attempt finished", "channel_matched", matchedChannelName != "")

	if len(decryptedPlaintext) < 5 {
		slog.Warn("[Group Text] Dropping packet: decryption failed or plaintext too short")
		return
	}

	rawTimestamp := binary.LittleEndian.Uint32(decryptedPlaintext[0:4])
	txTime := time.Unix(int64(rawTimestamp), 0)

	timeWindow := 30 * time.Minute
	if time.Since(txTime) > timeWindow || time.Until(txTime) > timeWindow {
		slog.Warn("[Group Text] Dropping message: failed anti-replay freshness check")
		return
	}

	// Strip null padding added by AES block alignment
	// Skip 4-byte timestamp + 1-byte flags (plaintext format per MeshCore spec)
	msgBytes := decryptedPlaintext[5:]
	if i := bytes.IndexByte(msgBytes, 0); i >= 0 {
		msgBytes = msgBytes[:i]
	}

	successMsgPacket := GroupTextMessage{
		ChannelName: matchedChannelName,
		Timestamp:   txTime,
		Message:     string(msgBytes),
	}

	slog.Info("[Group Text] Broadcast Message Received",
		"channel", successMsgPacket.ChannelName,
		"time", successMsgPacket.Timestamp.Format("15:04:05"),
		"message", successMsgPacket.Message,
	)
}
