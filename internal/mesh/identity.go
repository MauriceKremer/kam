package mesh

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// Errors returned by identity and key-exchange operations.
var (
	ErrBadKey       = errors.New("mesh: invalid public key")
	ErrBadSecret    = errors.New("mesh: shared secret must be 32 bytes")
	ErrLowOrder     = errors.New("mesh: key exchange produced a low-order point")
	ErrNoFreeIdent  = errors.New("mesh: could not generate an acceptable identity")
	ErrBadSeedSize  = errors.New("mesh: identity seed must be 32 bytes")
	errNoRandomness = errors.New("mesh: random source is nil")
)

// maxIdentityAttempts bounds identity generation when the caller rejects
// hashes. A one-byte hash has 254 usable values, so this is never reached in
// practice.
const maxIdentityAttempts = 4096

// ValidHash reports whether a node hash (first public-key byte) is usable.
// MeshCore reserves 0x00 and 0xFF (LocalIdentity::validatePrivateKey).
func ValidHash(b byte) bool { return b != 0x00 && b != 0xFF }

// Identity is a MeshCore node identity: an Ed25519 key pair that also acts as
// the X25519 key-exchange key. The seed is the only secret that needs storing.
type Identity struct {
	seed ed25519.PrivateKey
	pub  [PubKeySize]byte
	ecdh *ecdh.PrivateKey
}

// NewIdentity derives an identity from a 32-byte Ed25519 seed. The key
// exchange scalar is the first half of SHA-512(seed), the same value MeshCore
// stores as the first 32 bytes of its 64-byte private key.
func NewIdentity(seed []byte) (*Identity, error) {
	if len(seed) != SeedSize {
		return nil, ErrBadSeedSize
	}
	priv := ed25519.NewKeyFromSeed(seed)
	scalar := sha512.Sum512(seed)
	xpriv, err := ecdh.X25519().NewPrivateKey(scalar[:32])
	if err != nil {
		return nil, fmt.Errorf("mesh: derive key exchange key: %w", err)
	}
	id := &Identity{seed: priv, ecdh: xpriv}
	copy(id.pub[:], priv.Public().(ed25519.PublicKey))
	return id, nil
}

// GenerateIdentity creates a random identity. Keys whose hash is reserved or
// rejected by the caller (for example because another room uses it) are
// regenerated.
func GenerateIdentity(random io.Reader, reject func(hash byte) bool) (*Identity, error) {
	if random == nil {
		return nil, errNoRandomness
	}
	for attempt := 0; attempt < maxIdentityAttempts; attempt++ {
		var seed [SeedSize]byte
		if _, err := io.ReadFull(random, seed[:]); err != nil {
			return nil, fmt.Errorf("mesh: read random seed: %w", err)
		}
		id, err := NewIdentity(seed[:])
		if err != nil {
			return nil, err
		}
		if !ValidHash(id.Hash()) || (reject != nil && reject(id.Hash())) {
			continue
		}
		return id, nil
	}
	return nil, ErrNoFreeIdent
}

// PublicKey returns the 32-byte Ed25519 public key.
func (id *Identity) PublicKey() [PubKeySize]byte { return id.pub }

// Hash returns the node hash: the first public-key byte.
func (id *Identity) Hash() byte { return id.pub[0] }

// Seed returns a copy of the secret seed for storage.
func (id *Identity) Seed() []byte { return append([]byte(nil), id.seed.Seed()...) }

// Sign returns an Ed25519 signature over message.
func (id *Identity) Sign(message []byte) []byte { return ed25519.Sign(id.seed, message) }

// SharedSecret computes the 32-byte secret shared with the holder of peer
// (an Ed25519 public key), byte-compatible with ed25519_key_exchange.
func (id *Identity) SharedSecret(peer []byte) ([SecretSize]byte, error) {
	var secret [SecretSize]byte
	u, err := edwardsToMontgomery(peer)
	if err != nil {
		return secret, err
	}
	remote, err := ecdh.X25519().NewPublicKey(u[:])
	if err != nil {
		return secret, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	out, err := id.ecdh.ECDH(remote)
	if err != nil {
		return secret, ErrLowOrder
	}
	copy(secret[:], out)
	return secret, nil
}

var fieldPrime = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))

// edwardsToMontgomery converts an Ed25519 public key (compressed Edwards y with
// a sign bit) to the X25519 u coordinate: u = (1+y)/(1-y) mod p. The sign bit
// is ignored, as in ed25519_key_exchange. The input is public data, so the
// non-constant-time big.Int arithmetic does not leak a secret.
func edwardsToMontgomery(pub []byte) ([32]byte, error) {
	var out [32]byte
	if len(pub) != PubKeySize {
		return out, ErrBadKey
	}
	var le [32]byte
	copy(le[:], pub)
	le[31] &= 0x7F
	y := littleEndianToInt(le[:])
	y.Mod(y, fieldPrime)

	numerator := new(big.Int).Add(y, big.NewInt(1))
	numerator.Mod(numerator, fieldPrime)
	denominator := new(big.Int).Sub(big.NewInt(1), y)
	denominator.Mod(denominator, fieldPrime)
	if denominator.Sign() == 0 {
		return out, ErrBadKey
	}
	denominator.ModInverse(denominator, fieldPrime)
	u := numerator.Mul(numerator, denominator)
	u.Mod(u, fieldPrime)
	intToLittleEndian(u, out[:])
	return out, nil
}

func littleEndianToInt(le []byte) *big.Int {
	be := make([]byte, len(le))
	for i, b := range le {
		be[len(le)-1-i] = b
	}
	return new(big.Int).SetBytes(be)
}

func intToLittleEndian(v *big.Int, out []byte) {
	be := v.Bytes()
	for i := range out {
		out[i] = 0
	}
	for i, b := range be {
		out[len(be)-1-i] = b
	}
}
