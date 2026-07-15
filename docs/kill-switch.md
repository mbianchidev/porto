# KillSwitch integration

Porto can optionally connect to [KillSwitch](https://github.com/mbianchidev/kill-switch) on macOS. The integration has three explicit capabilities:

- detect or install a compatible KillSwitch release;
- publish ports for processes the current Porto daemon is actively managing;
- ask KillSwitch to run its configured Dev cleanup pass.

The integration is disabled by default.

## Requirements

- macOS 13 or later;
- a KillSwitch release that installs `killswitchctl`;
- network access only when installing or updating KillSwitch.

Linux and Windows builds keep the setting disabled and report the integration as unsupported.

## Enable and install

Open Porto's dashboard, enable **KillSwitch**, and save the setting. Enabling does not install software automatically. If `killswitchctl` is missing, use **Install KillSwitch** or:

```sh
porto kill-switch install
```

Porto downloads the official installer from KillSwitch's `main` branch over HTTPS into a private temporary file, runs it in release mode, removes the file, and verifies the installed CLI before reporting success. The KillSwitch installer verifies the latest release checksum and installs the app under `~/bin`.

## Port ownership

Porto registers only the actual ports of processes held by the running daemon. Stopped projects, stale database entries, preferred ports that were not assigned, and Porto's own daemon/router ports are excluded.

KillSwitch stores Porto's ports under the source name `porto`, separately from the user's KillSwitch watch list. Repeated updates are coalesced so a newer process start or exit cannot be lost behind an in-progress sync.

Disabling the integration clears the `porto` source without modifying user-managed ports. Porto retries that clear the next time the daemon starts if KillSwitch was unavailable during disable.

## Commands

```sh
porto kill-switch status
porto kill-switch install
porto kill-switch sync
porto kill-switch cleanup
```

When the Porto daemon is running, these commands use its integration API. `sync` requires the daemon because only the daemon can identify processes it currently owns. `status`, `install`, and `cleanup` can run directly when the daemon is stopped.

`cleanup` uses KillSwitch's saved Dev cleanup policy. Porto does not override the auto-kill toggle, age threshold, runtime list, dev-server indicators, or protected exclusions. The command reports the number of candidates and terminated processes.

## Troubleshooting

- **missing**: Install or update KillSwitch so `~/bin/killswitchctl` exists.
- **unsupported**: Run the integration on macOS 13 or later.
- **error**: Read the status message, then retry **Sync active ports**. CLI stderr is kept separate from JSON output so native framework warnings do not corrupt the response.
- **busy**: Wait for the current install, sync, or cleanup operation to finish.
