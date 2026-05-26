package service

import "context"

// Mailer delivers transactional emails (OTP codes, password resets) for native
// mobile flows where there is no Next.js proxy to call Resend.
//
// The default NoopMailer satisfies the interface so the server compiles and
// runs without an email provider configured. To actually deliver mail on mobile
// flows, wire a real implementation (e.g. Resend-backed) in cmd/api/main.go.
type Mailer interface {
	SendOTP(ctx context.Context, email, code string) error
	SendPasswordReset(ctx context.Context, email, token string) error
}

// NoopMailer satisfies Mailer without sending anything. Logs are emitted by
// callers, not here, so the dependency stays inert.
type NoopMailer struct{}

func (NoopMailer) SendOTP(_ context.Context, _, _ string) error           { return nil }
func (NoopMailer) SendPasswordReset(_ context.Context, _, _ string) error { return nil }
