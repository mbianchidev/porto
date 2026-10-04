export type LogRecord = {
  timestamp?: string
  receivedAt?: string
  stream: 'stdout' | 'stderr' | 'combined'
  text: string
  partial?: boolean
}

export type BufferedLog = LogRecord & { sequence: number }

export function isLogRecord(value: unknown): value is LogRecord {
  return value !== null && typeof value === 'object'
    && 'text' in value && typeof value.text === 'string' && value.text.length <= 32768
    && 'stream' in value && ['stdout', 'stderr', 'combined'].includes(String(value.stream))
}

export class LogBuffer {
  private records: BufferedLog[] = []
  private start = 0
  private bytes = 0
  private sequence = 0
  dropped = 0

  push(record: LogRecord) {
    const entry = { ...record, sequence: this.sequence++ }
    this.records.push(entry)
    this.bytes += entry.text.length * 2
    while (this.records.length - this.start > 5000 || this.bytes > 4 * 1024 * 1024) {
      this.bytes -= this.records[this.start++].text.length * 2
      this.dropped++
    }
    if (this.start > 1000) {
      this.records = this.records.slice(this.start)
      this.start = 0
    }
  }

  snapshot() { return this.records.slice(this.start) }
}
