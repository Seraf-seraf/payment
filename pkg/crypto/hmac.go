package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// SHA256Hex возвращает SHA-256 hash строки в hex-формате.
func SHA256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// HMACSHA256Hex возвращает HMAC-SHA256 подпись сообщения в hex-формате.
func HMACSHA256Hex(secret string, message []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

// EqualHex сравнивает две hex-строки за константное время после декодирования.
func EqualHex(a, b string) bool {
	decodedA, errA := hex.DecodeString(a)
	decodedB, errB := hex.DecodeString(b)
	if errA != nil || errB != nil {
		return false
	}
	return hmac.Equal(decodedA, decodedB)
}

// EncryptSecret шифрует секрет через AES-GCM и возвращает значение с префиксом aesgcm.
func EncryptSecret(key, value string) (string, error) {
	block, err := aes.NewCipher(secretKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return "aesgcm:" + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptSecret расшифровывает aesgcm-секрет или возвращает plaintext для локальной совместимости.
func DecryptSecret(key, value string) (string, error) {
	if !strings.HasPrefix(value, "aesgcm:") {
		return value, nil
	}
	if key == "" {
		return "", errors.New("encryption key is required")
	}
	encoded := strings.TrimPrefix(value, "aesgcm:")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(secretKey(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("encrypted secret is too short")
	}
	nonce := data[:gcm.NonceSize()]
	ciphertext := data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func secretKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
