package main

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestValidateVacancy(t *testing.T) {
	valid := vacancyFields{
		title:          "Backend-разработчик",
		description:    "Сопровождение платформы обработки платежей.",
		employmentType: "full_time",
		workFormat:     "remote",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid minimal vacancy rejected: %v", err)
	}

	withSalary := valid
	withSalary.salaryMin, withSalary.salaryMax, withSalary.currency = "150000", "250000.50", "RUB"
	if err := withSalary.validate(); err != nil {
		t.Fatalf("valid salary rejected: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(*vacancyFields)
		error string
	}{
		{"empty title", func(f *vacancyFields) { f.title = "" }, "title"},
		{"long title", func(f *vacancyFields) { f.title = string(make([]byte, 121)) }, "title"},
		{"empty description", func(f *vacancyFields) { f.description = "" }, "description"},
		{"long description", func(f *vacancyFields) { f.description = string(make([]byte, 5001)) }, "description"},
		{"bad employment_type", func(f *vacancyFields) { f.employmentType = "shift" }, "employment_type"},
		{"bad work_format", func(f *vacancyFields) { f.workFormat = "hybrid-remote" }, "work_format"},
		{"long location", func(f *vacancyFields) { f.location = string(make([]byte, 121)) }, "location"},
		{"salary without currency", func(f *vacancyFields) { f.salaryMin = "150000" }, "currency"},
		{"bad salary format", func(f *vacancyFields) { f.salaryMin = "150,000"; f.currency = "RUB" }, "salary_min"},
		{"salary too precise", func(f *vacancyFields) { f.salaryMin = "150000.501"; f.currency = "RUB" }, "salary_min"},
		{"salary too large", func(f *vacancyFields) { f.salaryMin = "99999999999"; f.currency = "RUB" }, "salary_min"},
		{"lowercase currency", func(f *vacancyFields) { f.salaryMin = "150000"; f.currency = "rub" }, "currency"},
		{"min above max", func(f *vacancyFields) { f.salaryMin = "250000"; f.salaryMax = "150000"; f.currency = "RUB" }, "salary_min"},
		{"currency without salary", func(f *vacancyFields) { f.currency = "RUB" }, "currency"},
	}
	for _, tc := range cases {
		fields := valid
		tc.mut(&fields)
		err := fields.validate()
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
		} else if !contains(err.Error(), tc.error) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err.Error(), tc.error)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestValidUUID(t *testing.T) {
	for good := range map[string]bool{
		"0b8f4d34-8bb4-4d5f-9b3f-9a6b6c1d2e3f": true, // lowercase
		"0B8F4D34-8BB4-4D5F-9B3F-9A6B6C1D2E3F": true, // uppercase
	} {
		if !validUUID(good) {
			t.Errorf("validUUID(%q) = false", good)
		}
	}
	for _, bad := range []string{
		"", "not-a-uuid", "0b8f4d348bb44d5f9b3f9a6b6c1d2e3f", // no dashes
		"0b8f4d34-8bb4-4d5f-9b3f-9a6b6c1d2e3",   // short
		"0b8f4d34-8bb4-4d5f-9b3f-9a6b6c1d2e3f0", // long
		"0b8f4d34_8bb4-4d5f-9b3f-9a6b6c1d2e3f",  // bad dash position
		"0b8f4d34-8bb4-4d5f-9b3f-9a6b6c1d2e3x",  // non-hex
	} {
		if validUUID(bad) {
			t.Errorf("validUUID(%q) = true", bad)
		}
	}
}

func TestCursorRoundtrip(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 30, 45, 123456789, time.UTC)
	id := "0b8f4d34-8bb4-4d5f-9b3f-9a6b6c1d2e3f"
	token := encodeCursor(at, id)
	gotAt, gotID, err := decodeCursor(token)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !gotAt.Equal(at) || gotID != id {
		t.Fatalf("roundtrip mismatch: %v %q", gotAt, gotID)
	}

	badTimestamp := base64.RawURLEncoding.EncodeToString([]byte("2026-13-45T00:00:00Z|" + id))
	badID := base64.RawURLEncoding.EncodeToString([]byte("2026-10-29T00:00:00Z|not-a-uuid"))
	noParts := base64.RawURLEncoding.EncodeToString([]byte("no-separator"))
	for _, bad := range []string{"", "!!!", "not-base64!@#", badTimestamp, badID, noParts} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) accepted", bad)
		}
	}
}
