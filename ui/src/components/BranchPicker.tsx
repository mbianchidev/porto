import { useEffect, useId, useRef, useState } from 'react'

export function BranchPicker({
  label,
  value,
  options,
  defaultBranch,
  placeholder,
  disabled,
  onOpen,
  onSelect,
}: {
  label: string
  value: string
  options: string[]
  defaultBranch: string
  placeholder: string
  disabled: boolean
  onOpen: () => void
  onSelect: (branch: string) => void
}) {
  const id = useId()
  const inputID = `${id}-input`
  const listID = `${id}-list`
  const rootRef = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const pinned = [defaultBranch, 'main', 'master'].filter(Boolean)
  const orderedOptions = [...new Set(options)]
    .sort((left, right) => {
      const leftPinned = pinned.indexOf(left)
      const rightPinned = pinned.indexOf(right)
      if (leftPinned !== -1 || rightPinned !== -1) {
        if (leftPinned === -1) return 1
        if (rightPinned === -1) return -1
        return leftPinned - rightPinned
      }
      return left.localeCompare(right)
    })
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filteredOptions = orderedOptions.filter((branch) => (
    normalizedQuery === '' || branch.toLocaleLowerCase().includes(normalizedQuery)
  ))

  useEffect(() => {
    if (!open) return
    const close = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false)
        setQuery('')
      }
    }
    window.addEventListener('pointerdown', close)
    return () => window.removeEventListener('pointerdown', close)
  }, [open])

  function showOptions() {
    if (disabled) return
    onOpen()
    setQuery('')
    setOpen(true)
  }

  return (
    <div className="branchField">
      <label htmlFor={inputID}>{label}</label>
      <div className={`branchPicker ${open ? 'open' : ''}`} ref={rootRef}>
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="11" cy="11" r="6.5" />
          <path d="m16 16 4 4" />
        </svg>
        <input
          id={inputID}
          type="search"
          role="combobox"
          aria-autocomplete="list"
          aria-controls={listID}
          aria-expanded={open}
          value={open ? query : value}
          placeholder={placeholder}
          disabled={disabled}
          onFocus={showOptions}
          onClick={showOptions}
          onChange={(event) => {
            setQuery(event.target.value)
            setOpen(true)
          }}
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              setOpen(false)
              setQuery('')
              event.currentTarget.blur()
            }
            if (event.key === 'Enter' && filteredOptions.length === 1) {
              event.preventDefault()
              onSelect(filteredOptions[0])
              setOpen(false)
              setQuery('')
            }
          }}
        />
        <span className="branchChevron" aria-hidden="true">⌄</span>
        {open && (
          <div className="branchMenu" id={listID} role="listbox">
            {filteredOptions.length === 0 && (
              <span className="branchEmpty">No matching branches</span>
            )}
            {filteredOptions.map((branch) => (
              <button
                type="button"
                role="option"
                aria-selected={branch === value}
                className={branch === value ? 'selected' : ''}
                key={branch}
                onMouseDown={(event) => event.preventDefault()}
                onClick={() => {
                  onSelect(branch)
                  setOpen(false)
                  setQuery('')
                }}
              >
                <span>{branch}</span>
                {branch === defaultBranch && <small>default</small>}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
