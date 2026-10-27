package main

import (
	"strings"
	"testing"
	"time"
)

func TestPHCFormatRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("not a PHC argon2id hash: %q", hash)
	}
	if !verifyPassword("correct horse battery staple", hash) {
		t.Fatal("password did not verify")
	}
	if verifyPassword("wrong password", hash) {
		t.Fatal("wrong password verified")
	}
}

func TestPHCParamsAreStored(t *testing.T) {
	// A hash with different parameters must still verify: parameters live
	// inside the PHC string, so they can be raised without migration.
	hash := "$argon2id$v=19$m=65536,t=1,p=4$" +
		"RJ9bkaqa7QibWPkghdj4Dg$" +
		"Tm9MPRsvy3WyaVM2J0rhAaC3rgLJKimHPddDQ0jtxU0"
	if !verifyPassword("a long password", hash) {
		t.Fatal("hash with stored parameters did not verify")
	}
}

func TestPHCRejectsGarbage(t *testing.T) {
	for _, hash := range []string{
		"", "not-a-hash", "$argon2i$v=19$m=65536,t=2,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=0,t=2,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=2,p=2$!!!$aGFzaA",
	} {
		if verifyPassword("password", hash) {
			t.Fatalf("garbage hash verified: %q", hash)
		}
	}
}

func TestDummyHashMatchesFormat(t *testing.T) {
	if !strings.HasPrefix(dummyHash, "$argon2id$v=19$") {
		t.Fatalf("dummy hash has wrong format")
	}
	if verifyPassword("not-an-account-password", dummyHash) != true {
		t.Fatal("dummy hash must verify its own password for timing equalization")
	}
}

func TestRateLimiterWindowsAndSweep(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < 5; i++ {
		if !rl.Allow("k", 5, time.Minute) {
			t.Fatalf("attempt %d rejected", i)
		}
	}
	if rl.Allow("k", 5, time.Minute) {
		t.Fatal("6th attempt allowed")
	}
	// Expired keys are swept when the map grows.
	for i := 0; i < 5000; i++ {
		rl.Allow(string(rune(i)), 1, time.Nanosecond)
	}
	time.Sleep(time.Millisecond)
	rl.Allow("trigger-sweep", 1, time.Minute)
	if len(rl.attempts) > 5000+100 {
		t.Fatalf("map not swept: %d keys", len(rl.attempts))
	}
}
