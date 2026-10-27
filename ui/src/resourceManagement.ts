export const SAVER_MODES = ['disabled', 'engine-idle-suspend', 'container-pause', 'container-scale-to-zero'] as const
export type SaverMode = typeof SAVER_MODES[number]
export type SaverOverride = { mode?: string; idleSeconds?: number }
export type SaverConfig = {
  mode: SaverMode
  idleSeconds: number
  wakeSeconds: number
  maxPending: number
  maxBodyBytes: number
  suspendEngine: boolean
  exclusions: string[]
  overrides: Record<string, SaverOverride>
}
export type SaverEvent = { at: string; application: string; action: string; message?: string }
export type SaverStatus = {
  state: {
    config: SaverConfig
    entries: Record<string, { application: { id: string; name: string }; mode: string; phase: string; error?: string }>
    engineToken?: string
    events: SaverEvent[]
    message?: string
    pending: number
  }
  applications: Array<{ id: string; name: string; eligible: boolean; busy: boolean; reason?: string }>
  protocols: string[]
  rawTCP: boolean
}
export type EngineLimits = { cpus: number; memoryMiB: number; swapMiB: number | null; diskGiB: number }
export type EngineResourceStatus = {
  limits: EngineLimits
  supported: boolean
  restartRequired: boolean
  current?: { cpus: number; memory: number; disk: number; status: string; vmType: string }
  message?: string
}

export function isSaverMode(value: string): value is SaverMode {
  return SAVER_MODES.some((mode) => mode === value)
}

export function parseSaverOverrides(text: string): Record<string, SaverOverride> {
  const value: unknown = JSON.parse(text)
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Overrides must be a JSON object keyed by application ID.')
  const result: Record<string, SaverOverride> = {}
  for (const [id, override] of Object.entries(value)) {
    if (!override || typeof override !== 'object' || Array.isArray(override)) throw new Error(`Invalid override for ${id}.`)
    const mode: unknown = 'mode' in override ? override.mode : undefined
    const seconds: unknown = 'idleSeconds' in override ? override.idleSeconds : undefined
    if (mode !== undefined && mode !== 'disabled' && mode !== 'container-pause' && mode !== 'container-scale-to-zero') throw new Error(`Invalid suspension mode for ${id}.`)
    if (seconds !== undefined && (typeof seconds !== 'number' || !Number.isInteger(seconds) || seconds < 0 || seconds > 86400)) throw new Error(`Invalid idle timeout for ${id}.`)
    result[id] = { ...(typeof mode === 'string' ? { mode } : {}), ...(typeof seconds === 'number' ? { idleSeconds: seconds } : {}) }
  }
  return result
}
