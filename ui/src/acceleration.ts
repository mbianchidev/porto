export const ARCHITECTURE_POLICIES = ['automatic', 'native-only', 'rosetta-preferred', 'qemu-compatible'] as const
export type ArchitecturePolicy = typeof ARCHITECTURE_POLICIES[number]

export function isArchitecturePolicy(value: string): value is ArchitecturePolicy {
  return ARCHITECTURE_POLICIES.some((policy) => policy === value)
}

export type AccelerationSettingsValue = {
  policy: ArchitecturePolicy
  rosettaEnabled: boolean
}

type Capability = { supported: boolean; reason?: string }

export type AccelerationStatus = {
  settings: AccelerationSettingsValue
  capabilities: {
    hostOS: string
    hostArchitecture: string
    guestArchitecture?: string
    vmType?: string
    limaVersion?: string
    qemuVersion?: string
    virtualization: Capability
    rosetta: Capability
    rosettaHost: Capability
    binfmt: Capability
    amd64Handler?: string
  }
  message?: string
}
