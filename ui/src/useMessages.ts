import { useContext } from 'react'
import { MessagesContext, type MessagesContextValue } from './messagesContext'

export function useMessages(): MessagesContextValue {
  const context = useContext(MessagesContext)
  if (!context) throw new Error('useMessages must be used within a MessagesProvider')
  return context
}
