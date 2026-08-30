// Intentionally empty: the renderer loads the daemon's ordinary web UI and
// needs no Node or Electron capability exposed to it. Kept as an explicit,
// named preload (rather than omitting the option) so contextIsolation stays
// documented and any future bridged API is added here deliberately, not by
// relaxing nodeIntegration/contextIsolation in main.js.
