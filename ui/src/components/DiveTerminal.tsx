import { lazy, Suspense } from 'react'

const InteractiveTerminal = lazy(async () => {
  const terminal = await import('./VMTerminal')
  return { default: terminal.InteractiveTerminal }
})

function diveSocketURL(image: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/api/docker/images/${encodeURIComponent(image)}/dive`
}

export function DiveTerminal({ image }: { image: string }) {
  return (
    <>
      <section className="drawerPanel">
        <h3>Dive layer inspector</h3>
        <p className="hintLine">
          Explore files, layer contents, image history, wasted space, and efficiency without leaving Porto.
        </p>
        <p className="hintLine">
          Arrow keys navigate · Tab switches panes · Ctrl+L filters layers · Ctrl+A toggles aggregated changes · Q or Ctrl+C exits.
        </p>
      </section>
      <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading Dive…</div></section>}>
        <InteractiveTerminal
          key={image}
          endpoint={diveSocketURL(image)}
          title="Image layers"
          detail={image}
          running
          ariaLabel={`Dive layer inspector for ${image}`}
          stoppedMessage="Dive is unavailable for this image."
        />
      </Suspense>
    </>
  )
}
