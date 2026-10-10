package mesh

import (
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"io"
)

// Envelope sizes from MeshCore.h.
const (
	CipherKeySize   = 16
	CipherMACSize   = 2
	CipherBlockSize = 16
	SecretSize      = 32
)

// EncryptThenMAC zero-pads plaintext to the AES block size, encrypts it with
// AES-128-ECB (key = secret[0:16]) and prepends the first two bytes of
// HMAC-SHA-256 over the ciphertext (key = all 32 secret bytes). It mirrors
// Utils::encryptThenMAC and returns nil when secret is not 32 bytes.
func EncryptThenMAC(secret, plaintext []byte) []byte {
	if len(secret) != SecretSize {
		return nil
	}
	block, err := aes.NewCipher(secret[:CipherKeySize])
	if err != nil {
		return nil
	}
	padded := len(plaintext)
	if r := padded % CipherBlockSize; r != 0 {
		padded += CipherBlockSize - r
	}
	out := make([]byte, CipherMACSize+padded)
	cipher := out[CipherMACSize:]
	copy(cipher, plaintext)
	for i := 0; i < padded; i += CipherBlockSize {
		block.Encrypt(cipher[i:i+CipherBlockSize], cipher[i:i+CipherBlockSize])
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(cipher)
	copy(out[:CipherMACSize], mac.Sum(nil))
	return out
}

// MACThenDecrypt verifies the 2-byte MAC and decrypts the AES-128-ECB
// ciphertext. The returned plaintext keeps the zero padding. ok is false when
// the secret is not 32 bytes, the ciphertext is empty or not a whole number of
// blocks, or the MAC does not verify.
func MACThenDecrypt(secret, macAndCipher []byte) (plaintext []byte, ok bool) {
	if len(secret) != SecretSize || len(macAndCipher) < CipherMACSize+CipherBlockSize {
		return nil, false
	}
	cipher := macAndCipher[CipherMACSize:]
	if len(cipher)%CipherBlockSize != 0 {
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(cipher)
	if !hmac.Equal(mac.Sum(nil)[:CipherMACSize], macAndCipher[:CipherMACSize]) {
		return nil, false
	}
	block, err := aes.NewCipher(secret[:CipherKeySize])
	if err != nil {
		return nil, false
	}
	plaintext = make([]byte, len(cipher))
	for i := 0; i < len(cipher); i += CipherBlockSize {
		block.Decrypt(plaintext[i:i+CipherBlockSize], cipher[i:i+CipherBlockSize])
	}
	return plaintext, true
}

// PeerPayload builds the payload of a peer datagram (REQ, RESPONSE, TXT_MSG,
// PATH): dest_hash | src_hash | MAC | ciphertext.
func PeerPayload(destHash, srcHash byte, secret, plaintext []byte) ([]byte, error) {
	sealed := EncryptThenMAC(secret, plaintext)
	if sealed == nil {
		return nil, ErrBadSecret
	}
	payload := make([]byte, 0, 2+len(sealed))
	payload = append(payload, destHash, srcHash)
	payload = append(payload, sealed...)
	if len(payload) > MaxPacketPayload {
		return nil, ErrPayloadTooLong
	}
	return payload, nil
}

// AnonPayload builds the payload of an ANON_REQ: dest_hash | sender_pubkey |
// MAC | ciphertext. KAM only builds these in tests (it plays the client).
func AnonPayload(destHash byte, senderPub, secret, plaintext []byte) ([]byte, error) {
	if len(senderPub) != PubKeySize {
		return nil, ErrBadKey
	}
	sealed := EncryptThenMAC(secret, plaintext)
	if sealed == nil {
		return nil, ErrBadSecret
	}
	payload := make([]byte, 0, 1+PubKeySize+len(sealed))
	payload = append(payload, destHash)
	payload = append(payload, senderPub...)
	payload = append(payload, sealed...)
	if len(payload) > MaxPacketPayload {
		return nil, ErrPayloadTooLong
	}
	return payload, nil
}

// maxCombinedPath is the largest path+extra a PATH return can carry
// (MAX_PACKET_PAYLOAD - 2 - CIPHER_BLOCK_SIZE).
const maxCombinedPath = MaxPacketPayload - 2 - CipherBlockSize

// PathReturnPayload builds a PATH payload (Mesh::createPathReturn). path is the
// route as received with its encoded pathLen. With no extra, an 0xFF marker and
// four random bytes keep the packet hash unique.
func PathReturnPayload(destHash, srcHash byte, secret []byte, pathLen byte, path []byte, extraType byte, extra []byte, random io.Reader) ([]byte, error) {
	if !ValidPathLen(pathLen) || len(path) != int(pathLen&0x3F)*(int(pathLen>>6)+1) {
		return nil, ErrInvalidPath
	}
	if len(path)+len(extra)+5 > maxCombinedPath {
		return nil, ErrPayloadTooLong
	}
	data := make([]byte, 0, 1+len(path)+1+len(extra)+4)
	data = append(data, pathLen)
	data = append(data, path...)
	if len(extra) > 0 {
		data = append(data, extraType)
		data = append(data, extra...)
	} else {
		data = append(data, 0xFF)
		var blob [4]byte
		if _, err := io.ReadFull(random, blob[:]); err != nil {
			return nil, err
		}
		data = append(data, blob[:]...)
	}
	return PeerPayload(destHash, srcHash, secret, data)
}

// ParsePathReturn decodes the plaintext of a PATH payload. extra may be padded
// with zeroes. It mirrors the checks in Mesh::onRecvPacket.
func ParsePathReturn(plain []byte) (pathLen byte, path []byte, extraType byte, extra []byte, err error) {
	if len(plain) < 1 {
		return 0, nil, 0, nil, ErrTruncated
	}
	pathLen = plain[0]
	if !ValidPathLen(pathLen) {
		return 0, nil, 0, nil, ErrInvalidPath
	}
	pathBytes := int(pathLen&0x3F) * (int(pathLen>>6) + 1)
	if len(plain) < 1+pathBytes+1 {
		return 0, nil, 0, nil, ErrTruncated
	}
	path = append([]byte(nil), plain[1:1+pathBytes]...)
	extraType = plain[1+pathBytes] & 0x0F
	extra = append([]byte(nil), plain[2+pathBytes:]...)
	return pathLen, path, extraType, extra, nil
}
