package main

import (
	"strings"
	"testing"
)

func TestValidateResponse(t *testing.T) {
	if err := validateResponse("Здравствуйте! Готов обсудить роль.", ""); err != nil {
		t.Fatalf("valid response without contact rejected: %v", err)
	}
	if err := validateResponse("Здравствуйте!", "@candidate"); err != nil {
		t.Fatalf("valid response with contact rejected: %v", err)
	}

	cases := []struct {
		name        string
		coverLetter string
		contact     string
		error       string
	}{
		{"empty letter", "", "", "cover_letter"},
		{"long letter", strings.Repeat("a", 4001), "", "cover_letter"},
		{"short contact", "ok", "ab", "contact"},
		{"long contact", "ok", strings.Repeat("c", 201), "contact"},
	}
	for _, tc := range cases {
		err := validateResponse(tc.coverLetter, tc.contact)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
		} else if !contains(err.Error(), tc.error) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err.Error(), tc.error)
		}
	}
}
