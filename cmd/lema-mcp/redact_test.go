package main

import (
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "No secrets",
			input:    "why did we use postgres?",
			expected: "why did we use postgres?",
		},
		{
			name:     "Bearer token",
			input:    "Authorization: Bearer 1234567890abcdef",
			expected: "Authorization: [REDACTED]",
		},
		{
			name:     "API key assignment",
			input:    "we set api_key=sk-1234567890",
			expected: "we set [REDACTED]",
		},
		{
			name:     "API key with quotes",
			input:    `we used api_key="12345" for this`,
			expected: `we used [REDACTED]" for this`, // The trailing quote is not matched by \s*...
		},
		{
			name:     "GitHub token",
			input:    "my token is ghp_123456789012345678901234567890123456",
			expected: "my [REDACTED]",
		},
		{
			name:     "Stripe key",
			input:    "stripe key sk-1234567890abcdefghij123456",
			expected: "stripe key [REDACTED]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactSecrets(tt.input)
			if got != tt.expected {
				t.Errorf("redactSecrets() = %v, want %v", got, tt.expected)
			}
		})
	}
}
