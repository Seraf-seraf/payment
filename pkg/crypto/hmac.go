package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func SHA256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func HMACSHA256Hex(secret string, message []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

func EqualHex(a, b string) bool {
	decodedA, errA := hex.DecodeString(a)
	decodedB, errB := hex.DecodeString(b)
	if errA != nil || errB != nil {
		return false
	}
	return hmac.Equal(decodedA, decodedB)
}
