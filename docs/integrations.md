# Optional integrations

Porto's optional integrations are disabled until enabled from the dashboard. They do not install or run when no compatible managed project is present.

## sql-not-so-lite

Enable [sql-not-so-lite](https://github.com/mbianchidev/sql-not-so-lite) from the dashboard's **Optional integration** panel. Porto checks managed project roots for files with SQLite extensions and validates the SQLite file header before doing external work.

If no orchestrated project contains a valid SQLite database, Porto does not install or run anything. When an eligible project exists, Porto uses an existing `sqnsl` binary from `PATH`, or installs the pinned integration revision with Go, then runs:

```sh
sqnsl scan <project-path>...
```

Daemon activation and rescans run in the background and expose their current state in the dashboard. Offline `porto scan` commands run the integration synchronously. Output and failures are recorded in eligible project logs under the `sqnsl` stream.

## KillSwitch

On macOS, enable [KillSwitch](https://github.com/mbianchidev/kill-switch) to share the active ports owned by processes managed by the current Porto daemon. KillSwitch keeps those source-owned ports separate from its own configured ports.

Installation is always explicit. Use **Install KillSwitch** in the dashboard or run:

```sh
porto kill-switch install
```

Porto can then sync active ports automatically and delegate cleanup to KillSwitch. Cleanup follows KillSwitch's auto-kill, age, runtime, indicator, and exclusion settings.

See the [KillSwitch integration guide](kill-switch.md) for requirements, commands, port ownership, and troubleshooting.

## Sendbox

Install [Sendbox](https://github.com/mbianchidev/sendbox) on a compatible macOS 26 Apple Silicon machine, then initialize each project that should expose the integration:

```sh
sendbox init --project /path/to/project
```

This creates `.sendbox.yaml`. Enable **Sendbox** in Porto's dashboard, then use **Run in Sendbox** and **Stop Sendbox**, or:

```sh
porto sendbox start <project>
porto sendbox stop <project>
```

Porto runs `sendbox run --config <project>/.sendbox.yaml --project <project>` and captures its output in the project's existing logs under the `sendbox` and `sendbox-stderr` streams. Porto does not install, require, or run Sendbox when no managed project contains `.sendbox.yaml`.

Sendbox sessions are independent from normal Porto processes. They do not receive Porto's automatic port assignment and are not routed through Porto's local hostnames. Avoid running both modes simultaneously when they would bind the same host port.
