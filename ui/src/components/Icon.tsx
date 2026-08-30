// Shared glyph set for rail navigation and action buttons. Every icon is a plain
// stroke-based line icon so it inherits `currentColor` and never introduces a new
// accent hue; color/meaning always comes from the surrounding lamp or label text.

export type IconName =
  | 'localhost'
  | 'open'
  | 'containers'
  | 'images'
  | 'builds'
  | 'volumes'
  | 'networks'
  | 'kubernetes'
  | 'pods'
  | 'services'
  | 'nodes'
  | 'machines'
  | 'activity'
  | 'settings'
  | 'play'
  | 'stop'
  | 'restart'
  | 'pause'
  | 'kill'
  | 'setup'
  | 'logs'
  | 'sendboxPlay'
  | 'sendboxStop'
  | 'cleanup'
  | 'remove'
  | 'pull'
  | 'build'
  | 'terminal'
  | 'file'
  | 'save'
  | 'snapshot'
  | 'restore'
  | 'create'
  | 'refresh'
  | 'search'
  | 'close'
  | 'chevronDown'
  | 'menu'
  | 'stats'
  | 'events'
  | 'manifest'

export function Icon({ name }: { name: IconName }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      {name === 'localhost' && <path d="M4 6h16v9H4Zm4 12h8M9 15v3m6-3v3" />}
      {name === 'open' && <><path d="M9 6H6a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-3" /><path d="M14 4h6v6M20 4l-9 9" /></>}
      {name === 'containers' && <path d="M4 7h16v13H4Zm0-3h16M8 4v3m8-3v3" />}
      {name === 'images' && <path d="M4 5h16v14H4Zm3 10 4-5 3 3.5 2-2.5 4 4M8.5 9.5a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" />}
      {name === 'builds' && <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Zm0 0v9m0 0-8-4.5M12 12l8-4.5" />}
      {name === 'volumes' && <path d="M4 8c0-2 3.5-3 8-3s8 1 8 3-3.5 3-8 3-8-1-8-3Zm0 0v8c0 2 3.5 3 8 3s8-1 8-3V8M4 12c0 2 3.5 3 8 3s8-1 8-3" />}
      {name === 'networks' && <path d="M12 4v5m0 0-6 4m6-4 6 4M6 13v4m12-4v4M4 21h4v-4H4Zm8 0h4v-4h-4Zm8 0h4v-4h-4Z" />}
      {name === 'kubernetes' && <path d="m12 3 7.5 4.3v9.4L12 21l-7.5-4.3V7.3ZM12 3v18m7.5-13.7L12 12m-7.5-2.7L12 12" />}
      {name === 'pods' && <path d="M12 3a4 4 0 0 1 4 4v3H8V7a4 4 0 0 1 4-4Zm-5 7h10l1 10H6Z" />}
      {name === 'services' && <path d="M12 6a2.5 2.5 0 1 0 0 5 2.5 2.5 0 0 0 0-5Zm0-3v2m0 14v2m8.5-11h-2m-13 0H3M17.7 6.3l-1.4 1.4m-8.6 8.6-1.4 1.4m11.4 0-1.4-1.4M7.7 7.7 6.3 6.3" />}
      {name === 'nodes' && <path d="M5 5h5v5H5Zm9 0h5v5h-5ZM5 14h5v5H5Zm9 0h5v5h-5Z" />}
      {name === 'machines' && <path d="M4 5h16v10H4Zm4 14h8m-4-4v4M4 9h16" />}
      {name === 'activity' && <path d="M3 12h4l2-7 4 14 2-7h6" />}
      {name === 'settings' && <path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Zm8-3.5a8 8 0 0 1-.1 1.2l2 1.6-2 3.4-2.3-.9a8 8 0 0 1-2 1.2L15 21H9l-.6-2.4a8 8 0 0 1-2-1.2l-2.3.9-2-3.4 2-1.6A8 8 0 0 1 4 12a8 8 0 0 1 .1-1.2l-2-1.6 2-3.4 2.3.9a8 8 0 0 1 2-1.2L9 3h6l.6 2.5a8 8 0 0 1 2 1.2l2.3-.9 2 3.4-2 1.6c.067.394.1.795.1 1.2Z" />}
      {name === 'play' && <path d="m8 5 11 7-11 7Z" />}
      {name === 'stop' && <rect x="6" y="6" width="12" height="12" rx="2" />}
      {name === 'restart' && <><path d="M20 11a8 8 0 1 0-2.34 5.66" /><path d="M20 4v7h-7" /></>}
      {name === 'pause' && <><rect x="6" y="5" width="4" height="14" rx="1" /><rect x="14" y="5" width="4" height="14" rx="1" /></>}
      {name === 'kill' && <><path d="M8.5 3h7l4.5 5v8l-4.5 5h-7L4 16V8Z" /><path d="m9 9 6 6m0-6-6 6" /></>}
      {name === 'setup' && <><path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" /><path d="m4.3 7.7 7.7 4.2 7.7-4.2M12 12v9" /></>}
      {name === 'logs' && <><path d="M5 5h14v14H5Z" /><path d="m8 9 2 2-2 2m4 1h4" /></>}
      {name === 'sendboxPlay' && <><path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" /><path d="m4.3 7.7 7.7 4.2 7.7-4.2M12 12v9" /><path d="m10 8 4 2.2-4 2.3Z" /></>}
      {name === 'sendboxStop' && <><path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" /><rect x="9.5" y="8.5" width="5" height="5" rx="0.5" /></>}
      {name === 'cleanup' && <><path d="M7 4v5a3 3 0 0 0 3 3h7" /><path d="m14 9 3 3-3 3" /><path d="M7 20v-3a3 3 0 0 1 3-3" /></>}
      {name === 'remove' && <><path d="M5 7h14M9 7V4h6v3m-8 0 1 13h8l1-13" /><path d="M10 11v5m4-5v5" /></>}
      {name === 'pull' && <><path d="M12 3v13m0 0-4-4m4 4 4-4" /><path d="M5 21h14" /></>}
      {name === 'build' && <path d="m4 17 6-11 4 7 3-4 3 8Z" />}
      {name === 'terminal' && <><path d="M5 5h14v14H5Z" /><path d="m8 10 3 2-3 2m5 0h4" /></>}
      {name === 'file' && <><path d="M7 3h7l4 4v14H7Z" /><path d="M14 3v4h4" /></>}
      {name === 'save' && <><path d="M5 5h11l3 3v11H5Z" /><path d="M8 5v5h8V5M8 14h8v5H8Z" /></>}
      {name === 'snapshot' && <><circle cx="12" cy="12" r="7" /><path d="M12 8.5v3.5l2.4 1.4" /></>}
      {name === 'restore' && <><path d="M4 12a8 8 0 1 0 2.6-5.9" /><path d="M4 4v4h4" /></>}
      {name === 'create' && <path d="M12 5v14M5 12h14" />}
      {name === 'refresh' && <><path d="M20 11a8 8 0 1 0-2.34 5.66" /><path d="M20 4v7h-7" /></>}
      {name === 'search' && <><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></>}
      {name === 'close' && <path d="M6 6 18 18M18 6 6 18" />}
      {name === 'chevronDown' && <path d="m8 10 4 4 4-4" />}
      {name === 'menu' && <path d="M4 6h16M4 12h16M4 18h16" />}
      {name === 'stats' && <path d="M5 19V9m6 10V5m6 14v-7" />}
      {name === 'events' && <path d="M12 8v5l3 2M12 3a9 9 0 1 0 9 9" />}
      {name === 'manifest' && <><path d="M7 3h10v18H7Z" /><path d="M9 8h6M9 12h6M9 16h4" /></>}
    </svg>
  )
}
