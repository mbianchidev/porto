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
    && 'stream' in value && typeof value.stream === 'string' && ['stdout', 'stderr', 'combined'].includes(value.stream)
    && (!('timestamp' in value) || validTimestamp(value.timestamp))
    && (!('receivedAt' in value) || validTimestamp(value.receivedAt))
    && (!('partial' in value) || typeof value.partial === 'boolean')
}

function validTimestamp(value: unknown): boolean {
  return value === undefined || typeof value === 'string' && value.length <= 64 && Number.isFinite(Date.parse(value))
}

export class LogBuffer {
  private records: Array<BufferedLog | undefined> = []
  private start = 0
  private bytes = 0
  private sequence = 0
  dropped = 0

  push(record: LogRecord) {
    const entry: BufferedLog = {
      stream: record.stream, text: record.text, timestamp: record.timestamp,
      receivedAt: record.receivedAt, partial: record.partial, sequence: this.sequence++,
    }
    this.records.push(entry)
    this.bytes += entry.text.length * 2
    while (this.records.length - this.start > 5000 || this.bytes > 4 * 1024 * 1024) {
      const oldest = this.records[this.start]
      if (!oldest) throw new Error('Log buffer lost its oldest record')
      this.bytes -= oldest.text.length * 2
      this.records[this.start++] = undefined
      this.dropped++
    }
    if (this.start > 1000) {
      this.records = this.records.slice(this.start)
      this.start = 0
    }
  }

  snapshot() { return this.records.slice(this.start).filter((record): record is BufferedLog => record !== undefined) }

  clear() {
    this.records = []
    this.start = 0
    this.bytes = 0
    this.dropped = 0
  }
}
