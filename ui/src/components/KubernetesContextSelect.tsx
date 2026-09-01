import type { KubernetesContext } from '../types'

export function KubernetesContextSelect({
  contexts,
  value,
  onChange,
}: {
  contexts: KubernetesContext[]
  value: string
  onChange: (context: string) => void
}) {
  return (
    <label className="namespaceField contextField">
      <span>Context</span>
      <select
        value={value}
        disabled={contexts.length === 0}
        onChange={(event) => onChange(event.target.value)}
      >
        {contexts.length === 0 && <option value="">No contexts</option>}
        {contexts.map((context) => (
          <option key={context.name} value={context.name}>{context.name}</option>
        ))}
      </select>
    </label>
  )
}
