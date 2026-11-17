// Tray controls for Porto's Docker engine. The daemon remains the only owner of
// the engine; these items only call its start/stop routes and mirror its status.

const DOCKER_ENGINE_STATUS_MENU_ID = 'porto-docker-engine-status'
const DOCKER_ENGINE_TOGGLE_MENU_ID = 'porto-docker-engine-toggle'

const STATUS_LABELS = {
  checking: 'Checking...',
  running: 'Running',
  stopped: 'Stopped (rest mode)',
  starting: 'Starting...',
  stopping: 'Stopping...',
  disabled: 'Disabled in Settings',
  unavailable: 'Daemon unavailable',
}

function dockerEngineState(status) {
  if (status?.enabled !== true) return 'disabled'
  return status.available === true ? 'running' : 'stopped'
}

function dockerEngineStatusLabel(state) {
  return `Docker engine: ${STATUS_LABELS[state] ?? STATUS_LABELS.unavailable}`
}

function dockerEngineMenuItems(state, toggle) {
  return [
    { id: DOCKER_ENGINE_STATUS_MENU_ID, label: dockerEngineStatusLabel(state), enabled: false },
    {
      id: DOCKER_ENGINE_TOGGLE_MENU_ID,
      label: state === 'running' || state === 'stopping' ? 'Stop Docker engine' : 'Start Docker engine',
      enabled: state === 'running' || state === 'stopped',
      click: toggle,
    },
  ]
}

module.exports = {
  DOCKER_ENGINE_STATUS_MENU_ID,
  DOCKER_ENGINE_TOGGLE_MENU_ID,
  dockerEngineMenuItems,
  dockerEngineState,
  dockerEngineStatusLabel,
}
