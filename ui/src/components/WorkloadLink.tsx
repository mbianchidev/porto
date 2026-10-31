import type { ReactNode } from 'react'
import { SOURCE_HOST_URL_NOTICE, workloadURL } from '../environment'

export function WorkloadLink({ url, children }: { url: string; children: ReactNode }) {
  const href = workloadURL(url)
  return href ? <a href={href} target="_blank" rel="noreferrer">{children}</a>
    : <span title={SOURCE_HOST_URL_NOTICE}>{children} <small>(source host only)</small></span>
}
