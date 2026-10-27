package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
)

type mailSender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// captureMailer records messages instead of sending them. Used in tests.
type captureMailer struct {
	mu   sync.Mutex
	sent []capturedMail
}

type capturedMail struct {
	To, Subject, Body string
}

func (m *captureMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, capturedMail{to, subject, body})
	return nil
}

type logMailer struct{}

func (m logMailer) Send(_ context.Context, to, subject, body string) error {
	// Tokens appear only in the local development log; a real provider
	// replaces this implementation without changing callers. Body is logged
	// as a single line so tokens can be extracted without JSON parsing.
	slog.Info("dev mail", "to", to, "subject", subject, "body", strings.ReplaceAll(body, "\n", " "))
	return nil
}

type disabledMailer struct{}

func (m disabledMailer) Send(_ context.Context, _, _, _ string) error {
	return nil
}

func mailerFromEnv() mailSender {
	if os.Getenv("DEV_MAIL_LOG") == "true" {
		return logMailer{}
	}
	return disabledMailer{}
}
