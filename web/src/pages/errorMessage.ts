// Error text for the operator.
//
// The API client raises the server's own message when the response carried
// one — "a bookmark may have at most 20 tags", "semantic search is disabled"
// — and those sentences are exactly what should appear on screen. Transport
// failures and non-JSON responses instead raise developer-facing text that
// names the endpoint and the HTTP status; that tells an operator nothing and
// leaks internal paths into the UI, so those get a plain fallback instead.
const developerText = /\bHTTP \d{3}\b|\/api\/v1\b/;

export function errorMessage(cause: unknown, fallback: string): string {
  const message = cause instanceof Error ? cause.message.trim() : "";
  if (!message || developerText.test(message)) return fallback;
  return message;
}
