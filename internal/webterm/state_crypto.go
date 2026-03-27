package webterm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const stateEncPrefix = "enc:v1:"

type stateCodec struct {
	aead cipher.AEAD
}

func newStateCodec(rawKey string) (*stateCodec, error) {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return nil, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, errors.New("dashboard encryption key must be base64")
	}
	if len(decoded) == 0 {
		return nil, errors.New("dashboard encryption key is empty")
	}

	key := sha256.Sum256(decoded)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	return &stateCodec{aead: aead}, nil
}

func (c *stateCodec) Encrypt(plain string) (string, error) {
	if c == nil {
		return plain, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nil, nonce, []byte(plain), nil)
	return stateEncPrefix + base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *stateCodec) Decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, stateEncPrefix) {
		return stored, nil
	}
	if c == nil {
		return "", errors.New("dashboard state is encrypted but no key is configured")
	}
	parts := strings.SplitN(strings.TrimPrefix(stored, stateEncPrefix), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid encrypted state format")
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	opened, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(opened), nil
}
