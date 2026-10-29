import { errorMessage } from '../api'
import { ADMIN_TARGET_KEY, type AdminTarget } from '../kubeAdminTypes'
import { useMessages } from '../useMessages'

export function KubernetesAdminLink({ target }: { target: AdminTarget }) {
  const { notifyError } = useMessages()
  return <a href="#/kubernetes-admin" onClick={(event) => {
    try { window.sessionStorage.setItem(ADMIN_TARGET_KEY, JSON.stringify(target)) } catch (error) {
      event.preventDefault()
      notifyError('kubernetes', errorMessage(error, 'Unable to transfer the explicit resource selection to administration'))
    }
  }}>Administer this resource</a>
}
