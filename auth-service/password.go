package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Default Argon2id parameters. Stored inside each PHC hash, so they can be
// raised later without a migration: verification uses the stored parameters.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// dummyHash is computed once at startup to equalize timing for unknown accounts.
var dummyHash string

func init() {
	dummyHash, _ = hashPassword("not-an-account-password")
}

// hashPassword returns a PHC string ($argon2id$v=19$m=...,t=...,p=...$salt$hash).
// The format is readable by both Go and Rust argon2 libraries.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

func verifyPassword(password, phc string) bool {
	params, salt, hash, err := parsePHC(phc)
	if err != nil {
		return false
	}
	candidate := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(hash)))
	return subtle.ConstantTimeCompare(hash, candidate) == 1
}

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
}

func parsePHC(phc string) (argonParams, []byte, []byte, error) {
	var params argonParams
	// $argon2id$v=19$m=65536,t=2,p=2$salt$hash
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return params, nil, nil, errors.New("unsupported hash format")
	}
	for _, kv := range strings.Split(parts[3], ",") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return params, nil, nil, errors.New("invalid parameters")
		}
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return params, nil, nil, errors.New("invalid parameters")
		}
		switch key {
		case "m":
			params.memory = uint32(n)
		case "t":
			params.time = uint32(n)
		case "p":
			params.threads = uint8(n)
		default:
			return params, nil, nil, errors.New("unknown parameter")
		}
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return params, nil, nil, errors.New("incomplete parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, err
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params, nil, nil, err
	}
	return params, salt, hash, nil
}
