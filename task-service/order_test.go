package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func TestValidateOrder(t *testing.T) {
	valid := orderFields{
		title:        "Лендинг для кофейни",
		description:  "Одностраничный сайт с формой заказа.",
		budgetType:   "fixed",
		budgetAmount: "120000",
		currency:     "RUB",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid minimal order rejected: %v", err)
	}
	withDeadline := valid
	withDeadline.deadline = "2026-12-31"
	if err := withDeadline.validate(); err != nil {
		t.Fatalf("valid deadline rejected: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(*orderFields)
		error string
	}{
		{"empty title", func(f *orderFields) { f.title = "" }, "title"},
		{"long title", func(f *orderFields) { f.title = strings.Repeat("t", 121) }, "title"},
		{"empty description", func(f *orderFields) { f.description = "" }, "description"},
		{"bad budget_type", func(f *orderFields) { f.budgetType = "monthly" }, "budget_type"},
		{"zero budget", func(f *orderFields) { f.budgetAmount = "0" }, "budget_amount"},
		{"negative budget", func(f *orderFields) { f.budgetAmount = "-100" }, "budget_amount"},
		{"bad budget format", func(f *orderFields) { f.budgetAmount = "120,000" }, "budget_amount"},
		{"lowercase currency", func(f *orderFields) { f.currency = "rub" }, "currency"},
		{"empty currency", func(f *orderFields) { f.currency = "" }, "currency"},
		{"bad deadline", func(f *orderFields) { f.deadline = "31.12.2026" }, "deadline"},
		{"impossible deadline", func(f *orderFields) { f.deadline = "2026-02-30" }, "deadline"},
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

func TestValidateProposal(t *testing.T) {
	if err := validateProposal("Сделаю за неделю.", "90000", "RUB", ""); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	if err := validateProposal("Сделаю.", "90000.50", "USD", "2026-12-31"); err != nil {
		t.Fatalf("valid proposal with deadline rejected: %v", err)
	}

	cases := []struct {
		name     string
		message  string
		amount   string
		currency string
		deadline string
		error    string
	}{
		{"empty message", "", "90000", "RUB", "", "message"},
		{"long message", strings.Repeat("m", 4001), "90000", "RUB", "", "message"},
		{"zero amount", "ok", "0", "RUB", "", "proposed_amount"},
		{"bad amount", "ok", "ninety", "RUB", "", "proposed_amount"},
		{"bad currency", "ok", "90000", "RU", "", "currency"},
		{"bad deadline", "ok", "90000", "RUB", "tomorrow", "proposed_deadline"},
	}
	for _, tc := range cases {
		err := validateProposal(tc.message, tc.amount, tc.currency, tc.deadline)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
		} else if !contains(err.Error(), tc.error) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err.Error(), tc.error)
		}
	}
}

func TestValidUUID(t *testing.T) {
	if !validUUID("019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f") {
		t.Fatal("valid UUID rejected")
	}
	for _, bad := range []string{"", "not-a-uuid", "019a2b7e7c3a7b1a8f0e1a2b3c4d5e6f",
		"019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6g", "019A2B7E-7C3A-7B1A-8F0E-1A2B3C4D5E6F-extra"} {
		if validUUID(bad) {
			t.Errorf("%q accepted as UUID", bad)
		}
	}
}

func TestValidDate(t *testing.T) {
	if !validDate("") {
		t.Fatal("empty optional date rejected")
	}
	if !validDate("2026-12-31") {
		t.Fatal("valid ISO date rejected")
	}
	for _, bad := range []string{"2026-13-01", "2026-12-32", "31-12-2026", "2026-12-31T00:00:00Z", "2026-1-1"} {
		if validDate(bad) {
			t.Errorf("%q accepted as a date", bad)
		}
	}
}

func TestCursorRoundtrip(t *testing.T) {
	at := time.Date(2026, 10, 30, 12, 34, 56, 789, time.UTC)
	id := "019a2b7e-7c3a-7b1a-8f0e-1a2b3c4d5e6f"
	token := encodeCursor(at, id)
	gotAt, gotID, err := decodeCursor(token)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if !gotAt.Equal(at) || gotID != id {
		t.Fatalf("roundtrip mismatch: %v %q", gotAt, gotID)
	}
	for _, bad := range []string{"", "!!!", base64.RawURLEncoding.EncodeToString([]byte("no-pipe")),
		base64.RawURLEncoding.EncodeToString([]byte("not-a-time|" + id)),
		base64.RawURLEncoding.EncodeToString([]byte("2026-10-30T12:34:56Z|not-a-uuid"))} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("bad cursor %q accepted", bad)
		}
	}
}
