import type { ButtonHTMLAttributes } from 'react'
import { Icon, type IconName } from './Icon'

type ActionButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  label: string
  icon: IconName
}

/** Icon-only quick action with a visible tooltip, aria-label, and hidden text label. */
export function ActionButton({ label, icon, className = '', ...props }: ActionButtonProps) {
  return (
    <button
      {...props}
      className={`iconButton ${className}`.trim()}
      type="button"
      aria-label={label}
      data-tooltip={label}
    >
      <Icon name={icon} />
      <span className="visuallyHidden">{label}</span>
    </button>
  )
}
