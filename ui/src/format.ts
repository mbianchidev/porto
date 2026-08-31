// Formats a Go time.Time ISO string as a short relative label. Go's zero time
// ("0001-01-01T00:00:00Z") serializes even through `omitempty` (structs are
// never considered empty by encoding/json), so callers pass fields such as
// lastStarted through here to render "never" instead of a 2000-year-old date.
export function formatRelativeTime(iso: string | undefined): string {
  if (!iso) return 'never'
  const date = new Date(iso)
  if (Number.isNaN(date.getTime()) || date.getFullYear() <= 1) return 'never'
  const diffSeconds = Math.round((Date.now() - date.getTime()) / 1000)
  if (diffSeconds < 5) return 'just now'
  if (diffSeconds < 60) return `${diffSeconds}s ago`
  const diffMinutes = Math.round(diffSeconds / 60)
  if (diffMinutes < 60) return `${diffMinutes}m ago`
  const diffHours = Math.round(diffMinutes / 60)
  if (diffHours < 24) return `${diffHours}h ago`
  const diffDays = Math.round(diffHours / 24)
  return `${diffDays}d ago`
}

// eslint-disable-next-line no-control-regex
const ANSI_ESCAPE_PATTERN = /\x1B(?:\[[0-9;?]*[a-zA-Z]|\][^\x07]*(?:\x07|\x1B\\)|[@-Z\\-_])/g
// eslint-disable-next-line no-control-regex
const OTHER_CONTROL_PATTERN = /[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/g

/**
 * Strips ANSI escape sequences (cursor movement, color) and other non-printing
 * control bytes from a live PTY stream, so the terminal renders as the same
 * plain engraved mono text as every other Porto readout instead of raw escape
 * codes or ad-hoc color noise.
 */
export function stripTerminalNoise(chunk: string): string {
  return chunk.replace(ANSI_ESCAPE_PATTERN, '').replace(OTHER_CONTROL_PATTERN, '')
}
