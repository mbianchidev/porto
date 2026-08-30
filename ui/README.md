# Porto control surface

The React app is a dense, hash-routed operations desk with a stable left rail,
a central ranked inventory per section, and a right-hand inspector for the
selected item (an accessible full-screen overlay on narrow widths):

- **Local development** — `#/localhost-ing` (default route) is the Porto
  project control board: fleet health, compact channel rows, quick actions,
  and an inspector with routing, branch, maintenance, and log console detail.
- **Containers** — `#/containers`, `#/images`, `#/builds`, `#/volumes`,
  `#/networks` inventory and operate the local Docker engine.
- **Kubernetes** — `#/kubernetes` (overview + context picker), `#/pods`
  (Overview/Logs/Terminal/Files/Stats/Events/Manifest inspector tabs),
  `#/services`, `#/nodes`.
- **Virtual machines** — `#/machines` lists the Lima-backed VM image catalog
  and instances, with a create form and terminal/snapshot inspector tabs.
- **System** — `#/activity` shows recent client-side actions and errors for
  this session; `#/settings` keeps branch cleanup and integration settings.

Every runtime section (Docker, Kubernetes, VMs) reports a clear "unavailable"
state instead of inventing data when the underlying engine, cluster, or
hypervisor cannot be reached.

## Development

```sh
npm install
npm run dev
```

The Vite development server proxies `/api` and WebSocket upgrades to a Porto
daemon running at `127.0.0.1:37623`.

Run `npm run lint` and `npm run build` before submitting changes.

## Desktop shell

`electron/` contains a minimal Electron wrapper that loads this UI from the
Porto daemon (`http://127.0.0.1:37623`) and starts the daemon only when it is
unreachable. Install and run it from the `ui` directory:

```sh
npm run desktop:install
npm run desktop
```

See `electron/README.md` for packaging.
