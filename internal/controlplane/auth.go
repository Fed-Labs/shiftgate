package controlplane

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 320 || !emailPattern.MatchString(value) {
		return "", errors.New("a valid email address is required")
	}
	return value, nil
}

func validatePassword(value string) error {
	if len(value) < 12 {
		return errors.New("password must contain at least 12 characters")
	}
	if len(value) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}

func hashPassword(password, pepper string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	if len(pepper) < 32 {
		return "", errors.New("password pepper is not configured")
	}
	peppered := hmacSHA256([]byte(pepper), []byte(password))
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	derived := argon2.IDKey(peppered, salt, argonIterations, argonMemory, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(derived)), nil
}

func verifyPassword(password, encoded, pepper string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	parameters := map[string]uint32{}
	for _, parameter := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(parameter, "=")
		if !ok {
			return false
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return false
		}
		parameters[key] = uint32(parsed)
	}
	memory, okMemory := parameters["m"]
	iterations, okIterations := parameters["t"]
	parallelism, okParallelism := parameters["p"]
	if !okMemory || !okIterations || !okParallelism || memory < 16*1024 || memory > 1024*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 16 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != argonKeyBytes {
		return false
	}
	peppered := hmacSHA256([]byte(pepper), []byte(password))
	actual := argon2.IDKey(peppered, salt, iterations, memory, uint8(parallelism), uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func newToken(prefix string) (token string, raw []byte, err error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", nil, err
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(value)
	return token, value, nil
}

func tokenDigest(pepper, token string) []byte {
	return hmacSHA256([]byte(pepper), []byte(token))
}

func hmacSHA256(key, value []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

func secureCompareString(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func parseBearer(value string) (string, bool) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || len(parts[1]) < 20 || len(parts[1]) > 256 {
		return "", false
	}
	return parts[1], true
}

func validTokenExpiry(now, expiry time.Time) bool {
	return expiry.After(now.Add(5 * time.Second))
}
