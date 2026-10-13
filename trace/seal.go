package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the length of a session key.
const KeySize = chacha20poly1305.KeySize

// statePrefix marks a sealed agent state in the sessions table. A backend
// with no trace store configured has no session key and stores the state as
// plain agentkit.State JSON, so the two are told apart by looking rather than
// by guessing at the shape of what is there.
const statePrefix = "enc:v1:"

// NewKey mints a session key. It is the only key that encrypts anything here;
// the age recipients exist to carry copies of it.
func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// WrapKey encrypts a session key to an age recipient ("age1…") and returns it
// base64-encoded, ready to sit in a trace file's header.
func WrapKey(key []byte, recipient string) (string, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return "", fmt.Errorf("trace: bad recipient: %w", err)
	}
	var out bytes.Buffer
	enc, err := age.Encrypt(&out, r)
	if err != nil {
		return "", err
	}
	if _, err := enc.Write(key); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// UnwrapKey recovers a session key from a header entry. Only the holder of a
// matching private identity can do this — never the backend, which is handed
// the raw key by a client for as long as the session is live and holds no
// identity of its own.
func UnwrapKey(wrapped string, identity age.Identity) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(sealed), identity)
	if err != nil {
		return nil, err
	}
	key, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("trace: unwrapped key is %d bytes, want %d", len(key), KeySize)
	}
	return key, nil
}

// Seal encrypts one plaintext under a session key and returns
// base64(nonce || ciphertext).
//
// Each call generates a random nonce independently of record sequence numbers.
// Reusing a nonce with the same key compromises encryption.
func Seal(key, plain []byte) (string, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, nil)), nil
}

// Open reverses Seal.
func Open(key []byte, blob string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("trace: sealed blob is too short")
	}
	return aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
}

// SealState wraps the agent state on its way to the sessions table. The
// backend writes this while a session is live and cannot read it back after
// the key has left memory; a client returns the key on resume.
func SealState(key, plain []byte) (string, error) {
	blob, err := Seal(key, plain)
	if err != nil {
		return "", err
	}
	return statePrefix + blob, nil
}

// OpenState reverses SealState.
func OpenState(key []byte, state string) ([]byte, error) {
	if !IsSealedState(state) {
		return nil, errors.New("trace: state is not sealed")
	}
	return Open(key, strings.TrimPrefix(state, statePrefix))
}

// IsSealedState reports whether a stored session state needs a key. A backend
// with no trace store has none, and stores the conversation in the clear.
func IsSealedState(state string) bool {
	return strings.HasPrefix(state, statePrefix)
}
