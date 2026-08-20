package diagnostics

import (
	"regexp"
	"strings"
)

var (
	// Assignment-shaped credentials are the common form in formatted errors,
	// JSON snippets, URLs, and key/value logs.  The replacement intentionally
	// drops the original quoting so a secret can never survive in a quote edge
	// case.
	sensitiveAssignment = regexp.MustCompile(`(?i)(\b(?:api[_ -]?hash|bot[_ -]?token|access[_ -]?token|refresh[_ -]?token|client[_ -]?secret|proxy[_ -]?password|password|passwd|secret|authorization|auth|session(?:[_ -]?string)?|token)\b\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}\]]+)`)
	bearerCredential    = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;}\]]+`)
	telegramBotToken    = regexp.MustCompile(`\b\d{6,12}:[A-Za-z0-9_-]{20,}\b`)
)

// redact removes credential-shaped values from messages before they are
// serialized.  It is deliberately conservative about ordinary words and
// deliberately broad about well-known Telegram/API credential names.
func redact(message string) string {
	// Most diagnostic records contain only fixed lifecycle metadata. Avoid
	// running several regular expressions over large ordinary messages (for
	// example a rotation stress record) when their required markers are absent.
	lower := strings.ToLower(message)
	if strings.Contains(lower, "token") || strings.Contains(lower, "password") ||
		strings.Contains(lower, "passwd") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "session") || strings.Contains(lower, "auth") ||
		strings.Contains(lower, "api_hash") || strings.Contains(lower, "api hash") ||
		strings.Contains(lower, "api-hash") {
		message = sensitiveAssignment.ReplaceAllString(message, "$1[REDACTED]")
	}
	if strings.Contains(lower, "bearer") {
		message = bearerCredential.ReplaceAllString(message, "Bearer [REDACTED]")
	}
	if strings.Contains(message, ":") {
		message = telegramBotToken.ReplaceAllString(message, "[REDACTED]")
	}
	return message
}
