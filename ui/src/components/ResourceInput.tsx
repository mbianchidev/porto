import { useId } from 'react'
import type { InputHTMLAttributes } from 'react'

export function ResourceInput({ options, ...props }: Omit<InputHTMLAttributes<HTMLInputElement>, 'list'> & { options: readonly string[] }) {
  const id = useId()
  const names = [...new Set(options)].sort((left, right) => left.localeCompare(right))
  return <>
    <input {...props} list={id} />
    <datalist id={id}>{names.map((name) => <option key={name} value={name} />)}</datalist>
  </>
}
