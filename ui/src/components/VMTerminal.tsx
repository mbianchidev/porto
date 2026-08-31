import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import type { VMInstance } from '../types'
import { ActionButton } from './ActionButton'

const CONNECT_TIMEOUT_MS = 5000

type VMTerminalState = 'connecting' | 'open' | 'ended' | 'unavailable'

function terminalSocketURL(instanceName: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/api/vms/instances/${encodeURIComponent(instanceName)}/terminal`
}

type InteractiveTerminalProps = {
  endpoint: string
  title: string
  detail: string
  running: boolean
  ariaLabel: string
  stoppedMessage: string
}

export function InteractiveTerminal({
  endpoint,
  title,
  detail,
  running,
  ariaLabel,
  stoppedMessage,
}: InteractiveTerminalProps) {
  const [maximized, setMaximized] = useState(false)
  const [sessionToken, setSessionToken] = useState(0)
  const [state, setState] = useState<VMTerminalState>(
    running && typeof WebSocket !== 'undefined' ? 'connecting' : 'unavailable',
  )
  const terminalHostRef = useRef<HTMLDivElement | null>(null)
  const terminalRef = useRef<Terminal | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const resizeObserverRef = useRef<ResizeObserver | null>(null)

  const attachTerminalHost = useCallback((node: HTMLDivElement | null) => {
    terminalHostRef.current = node
    resizeObserverRef.current?.disconnect()
    if (!node) return
    const terminal = terminalRef.current
    if (terminal?.element && terminal.element.parentElement !== node) {
      node.append(terminal.element)
    }
    resizeObserverRef.current?.observe(node)
    window.requestAnimationFrame(() => {
      if (node.clientWidth > 0 && node.clientHeight > 0) fitAddonRef.current?.fit()
    })
  }, [])

  useEffect(() => {
    if (!maximized) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setMaximized(false)
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [maximized])

  useEffect(() => {
    if (!running || typeof WebSocket === 'undefined') return
    const host = terminalHostRef.current
    if (!host) return

    const styles = window.getComputedStyle(document.documentElement)
    const terminal = new Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
      fontSize: 12,
      lineHeight: 1.35,
      screenReaderMode: true,
      scrollback: 5000,
      theme: {
        background: styles.getPropertyValue('--panel-dark').trim(),
        foreground: styles.getPropertyValue('--white').trim(),
        cursor: styles.getPropertyValue('--amber').trim(),
        cursorAccent: styles.getPropertyValue('--panel-dark').trim(),
        selectionBackground: styles.getPropertyValue('--line-strong').trim(),
      },
    })
    const fitAddon = new FitAddon()
    terminal.loadAddon(fitAddon)
    terminal.open(host)
    terminalRef.current = terminal
    fitAddonRef.current = fitAddon

    const fitTerminal = () => {
      const currentHost = terminalHostRef.current
      if (currentHost && currentHost.clientWidth > 0 && currentHost.clientHeight > 0) fitAddon.fit()
    }
    const resizeObserver = new ResizeObserver(fitTerminal)
    resizeObserver.observe(host)
    resizeObserverRef.current = resizeObserver
    window.requestAnimationFrame(fitTerminal)

    let opened = false
    let disposed = false
    const encoder = new TextEncoder()
    const socket = new WebSocket(endpoint)
    socket.binaryType = 'arraybuffer'
    const sendResize = (cols = terminal.cols, rows = terminal.rows) => {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'resize', cols, rows }))
      }
    }
    const connectTimeout = window.setTimeout(() => {
      if (!opened) socket.close()
    }, CONNECT_TIMEOUT_MS)

    socket.onopen = () => {
      opened = true
      window.clearTimeout(connectTimeout)
      setState('open')
      fitTerminal()
      sendResize()
      terminal.focus()
    }
    socket.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        terminal.write(new Uint8Array(event.data))
      } else if (typeof event.data === 'string') {
        terminal.write(event.data)
      }
    }
    socket.onclose = (event) => {
      window.clearTimeout(connectTimeout)
      if (disposed) return
      setState(opened ? 'ended' : 'unavailable')
      const message = event.reason || (opened ? 'session ended' : 'connection unavailable')
      terminal.write(`\r\n\x1b[90m[${message}]\x1b[0m\r\n`)
    }
    const inputSubscription = terminal.onData((data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(encoder.encode(data))
    })
    const resizeSubscription = terminal.onResize(({ cols, rows }) => sendResize(cols, rows))

    return () => {
      disposed = true
      window.clearTimeout(connectTimeout)
      inputSubscription.dispose()
      resizeSubscription.dispose()
      resizeObserver.disconnect()
      socket.close()
      terminal.dispose()
      resizeObserverRef.current = null
      fitAddonRef.current = null
      terminalRef.current = null
    }
  }, [endpoint, running, sessionToken])

  const stateLabel = state === 'open'
    ? 'connected'
    : state === 'connecting'
      ? 'connecting…'
      : state === 'ended'
        ? 'session ended'
        : running
          ? 'unavailable'
          : 'VM stopped'

  const terminal = (
    <section className={`logConsole vmTerminal ${maximized ? 'terminalMaximized' : ''}`}>
      <div className="consoleHeader">
        <div><h3>{title}</h3><p>{detail} · {stateLabel}</p></div>
        <div className="consoleActions">
          {(state === 'ended' || state === 'unavailable') && running && (
            <ActionButton
              className="terminalWindowAction"
              label="Reconnect terminal"
              icon="restart"
              onClick={() => {
                setState('connecting')
                setSessionToken((value) => value + 1)
              }}
            />
          )}
          <ActionButton
            className="terminalWindowAction"
            label={maximized ? 'Minimize terminal' : 'Maximize terminal'}
            icon={maximized ? 'minimize' : 'maximize'}
            aria-pressed={maximized}
            onClick={() => setMaximized((value) => !value)}
          />
        </div>
      </div>
      {running
        ? <div className="xtermHost" ref={attachTerminalHost} aria-label={ariaLabel} />
        : <div className="terminalPlaceholder">{stoppedMessage}</div>}
    </section>
  )

  return maximized ? createPortal(terminal, document.body) : terminal
}

export default function VMTerminal({ instance }: { instance: VMInstance }) {
  return (
    <InteractiveTerminal
      endpoint={terminalSocketURL(instance.name)}
      title="Terminal"
      detail={instance.name}
      running={instance.status.toLocaleLowerCase() === 'running'}
      ariaLabel={`Interactive terminal for ${instance.name}`}
      stoppedMessage="Start the VM to open its terminal."
    />
  )
}
