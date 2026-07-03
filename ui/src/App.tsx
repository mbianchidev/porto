import { useEffect, useState } from 'react'
import './App.css'

type Project = {
  id: number
  name: string
  path: string
  strategy: string
  command: string
  port: number
  hostname: string
  pid: number
  status: string
  branch: string
  dirty: boolean
}

async function action(name: string, verb: string) {
  const response = await fetch(`/api/projects/${name}/${verb}`, { method: 'POST' })
  if (!response.ok) throw new Error(await response.text())
}

function App() {
  const [projects, setProjects] = useState<Project[]>([])
  const [error, setError] = useState('')

  async function refresh() {
    try {
      const response = await fetch('/api/projects')
      if (!response.ok) throw new Error(await response.text())
      setProjects(await response.json())
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to load projects')
    }
  }

  async function run(name: string, verb: string) {
    try {
      await action(name, verb)
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Action failed')
    }
  }

  useEffect(() => {
    refresh()
    const timer = window.setInterval(refresh, 5000)
    return () => window.clearInterval(timer)
  }, [])

  return (
    <main>
      <header className="hero">
        <div>
          <p className="eyebrow">Porto</p>
          <h1>Local Project Orchestrator</h1>
          <p>
            Discover runnable repos, start or stop them from one dashboard, and
            keep PID, port, logs, Git branch, and local hostnames in one small
            SQLite-backed daemon.
          </p>
        </div>
        <button type="button" onClick={refresh}>Refresh</button>
      </header>

      {error && <div className="error">{error}</div>}

      <section className="grid">
        {projects.length === 0 && (
          <article className="empty">
            <h2>No projects yet</h2>
            <p>Run <code>porto scan ~/code --depth 3</code> to discover projects.</p>
          </article>
        )}
        {projects.map((project) => (
          <article className="card" key={project.id}>
            <div className="cardHeader">
              <div>
                <h2>{project.name}</h2>
                <p>{project.path}</p>
              </div>
              <span className={`status ${project.status}`}>{project.status}</span>
            </div>

            <dl>
              <div><dt>Port</dt><dd>{project.port || 'unassigned'}</dd></div>
              <div><dt>PID</dt><dd>{project.pid || '—'}</dd></div>
              <div><dt>Branch</dt><dd>{project.branch}{project.dirty ? ' *' : ''}</dd></div>
              <div><dt>Strategy</dt><dd>{project.strategy}</dd></div>
              <div><dt>Host</dt><dd>{project.hostname}.porto.localhost:37680</dd></div>
            </dl>

            <code className="command">{project.command}</code>

            <div className="actions">
              <button type="button" onClick={() => run(project.name, 'start')}>Start</button>
              <button type="button" onClick={() => run(project.name, 'stop')}>Stop</button>
              <button type="button" onClick={() => run(project.name, 'restart')}>Restart</button>
              <button type="button" onClick={() => run(project.name, 'kill')}>Kill</button>
            </div>
          </article>
        ))}
      </section>
    </main>
  )
}

export default App
