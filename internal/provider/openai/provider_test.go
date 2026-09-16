package openai

import "testing"

func TestClampRetryAfter(t *testing.T) {
	tests := []struct {
		in, want string
		why      string
	}{
		{"5", "5", "small values pass through"},
		{"60", "60", "the limit itself is allowed"},
		// Claude Code aborts the turn rather than waiting longer than 60s, so
		// a provider's 3600 must not reach it.
		{"3600", "60", "oversized values are capped, not forwarded"},
		{"0", "0", "zero is a valid immediate retry"},
		{"Wed, 21 Oct 2026 07:28:00 GMT", "", "HTTP-date form is dropped: Claude Code parseInts it"},
		{"", "", "absent stays absent"},
		{"garbage", "", "junk is dropped"},
		{"-5", "", "negative is dropped"},
	}
	for _, tc := range tests {
		if got := clampRetryAfter(tc.in); got != tc.want {
			t.Errorf("clampRetryAfter(%q) = %q, want %q (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

func TestAnthropicErrorKind(t *testing.T) {
	// These names are Claude Code's classification vocabulary; a provider's
	// own error type must never appear here.
	tests := map[int]string{
		400: "invalid_request_error",
		401: "authentication_error",
		403: "permission_error",
		404: "not_found_error",
		413: "request_too_large",
		429: "rate_limit_error",
		500: "api_error",
		502: "api_error",
		529: "overloaded_error",
	}
	for status, want := range tests {
		if got := anthropicErrorKind(status); got != want {
			t.Errorf("anthropicErrorKind(%d) = %q, want %q", status, got, want)
		}
	}
}
