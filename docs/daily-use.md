# Daily use

Porto is designed to stay available as a localhost orchestrator while projects are started and stopped as needed. Install the release in a stable location, start the daemon at login or boot, and keep the same user and environment for both the daemon and CLI.

## Install in a stable location

### Linux and macOS

After verifying and extracting a [release archive](installation.md#install-a-release), copy it into a user-owned application directory:

```sh
mkdir -p "$HOME/.local/share/porto" "$HOME/.local/bin"
cp /path/to/extracted/porto "$HOME/.local/share/porto/porto"
cp -R /path/to/extracted/ui "$HOME/.local/share/porto/"
chmod 755 "$HOME/.local/share/porto/porto"
ln -sfn "$HOME/.local/share/porto/porto" "$HOME/.local/bin/porto"
```

Add the binary and dashboard location to your shell profile:

```sh
export PATH="$HOME/.local/bin:$PATH"
export PORTO_UI_DIR="$HOME/.local/share/porto/ui/dist"
```

Setting `PORTO_UI_DIR` explicitly keeps dashboard discovery predictable when the binary is reached through a symlink.

### Windows

Extract the full release into `%LOCALAPPDATA%\Porto` so it contains both `porto.exe` and `ui\dist`. Add `%LOCALAPPDATA%\Porto` to the user `Path`, then open a new terminal and verify:

```powershell
porto daemon status
```

Keep the archive layout intact whenever the installation is moved or upgraded.

## Start the daemon automatically

The daemon runs in the foreground and gracefully stops its managed projects when it receives the operating system's normal termination signal. Use the service manager for your platform rather than leaving a terminal open.

### Linux with a systemd user service

Create `~/.config/systemd/user/porto.service`:

```ini
[Unit]
Description=Porto local app orchestrator

[Service]
Type=simple
ExecStart=%h/.local/share/porto/porto daemon start
Environment=PORTO_UI_DIR=%h/.local/share/porto/ui/dist
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Extend `PATH` with the locations of any required language runtimes, package managers, or Docker client. Porto-launched projects inherit this environment.

Enable and start the service:

```sh
systemctl --user daemon-reload
systemctl --user enable --now porto
systemctl --user status porto
```

On an always-on home system, allow the user service to start at boot and continue without an interactive login:

```sh
sudo loginctl enable-linger "$USER"
```

This changes user-service scheduling only; Porto still runs as the unprivileged user. If project roots live on a mounted disk, add `RequiresMountsFor=/path/to/projects` under `[Unit]`.

### macOS with a LaunchAgent

Create `~/Library/LaunchAgents/dev.porto.daemon.plist`, replacing `/Users/YOU` with your home directory:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.porto.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/YOU/.local/share/porto/porto</string>
    <string>daemon</string>
    <string>start</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PORTO_UI_DIR</key>
    <string>/Users/YOU/.local/share/porto/ui/dist</string>
    <key>PATH</key>
    <string>/Users/YOU/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/Users/YOU/Library/Logs/porto-daemon.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/YOU/Library/Logs/porto-daemon.log</string>
</dict>
</plist>
```

Validate and load it:

```sh
plutil -lint "$HOME/Library/LaunchAgents/dev.porto.daemon.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/dev.porto.daemon.plist"
launchctl print "gui/$(id -u)/dev.porto.daemon"
```

The LaunchAgent starts when that user logs in. Add runtime directories to its `PATH` when projects depend on tools installed by a version manager.

### Windows with Task Scheduler

Create a task that:

1. triggers **At log on** for your user;
2. runs `%LOCALAPPDATA%\Porto\porto.exe`;
3. passes `daemon start` as its arguments;
4. starts in `%LOCALAPPDATA%\Porto`;
5. runs without elevated privileges and restarts on failure.

The working directory preserves dashboard asset discovery. Add required development tools to the user `Path` because managed projects inherit the task's environment.

## Everyday workflow

Scan long-lived project roots once:

```sh
porto scan ~/code ~/work --depth 3
```

Rescan after adding or moving repositories. On a typical day:

```sh
porto daemon status
porto list
porto start <project>
porto logs <project> --stream all -n 100
porto stop <project>
```

The dashboard at `http://127.0.0.1:37623` provides the same lifecycle controls plus dependency setup, logs, branch switching, and concurrent branch instances.

- Run **Setup dependencies** after the initial scan and when project dependencies change.
- Pin ports only for tools that need a fixed address; otherwise let Porto avoid collisions automatically.
- Leave the daemon running and stop projects you no longer need.
- Stop the service when you want Porto to gracefully stop every project it currently manages.

Starting the daemon does not automatically start every stored project. Start only the projects needed for the current development session.

## Use an always-on home system

Run Porto under an unprivileged account that owns or can access the project roots. Never run the daemon with `sudo`, because every managed project inherits the daemon's privileges.

Porto's dashboard and default routers bind to loopback. To administer a home system from another machine without publishing its control endpoints, create an SSH tunnel:

```sh
ssh -N \
  -L 37623:127.0.0.1:37623 \
  -L 37680:127.0.0.1:37680 \
  user@home-system
```

Then open the dashboard at `http://127.0.0.1:37623` and routed projects at `http://<project>.porto.localhost:37680` on the client machine. The SSH connection encrypts both routes.

Do not expose Porto's loopback control endpoints directly to a LAN or the internet. Keep them behind an SSH tunnel or another authenticated private-access layer.

## Upgrade and back up

Stop the service before replacing the binary and `ui/dist`; this gracefully stops managed projects. Copy the complete new release into the stable installation directory, then start the service again.

Porto keeps its database, certificates, managed worktrees, and other state in the platform's user configuration directory. Set `PORTO_HOME` to use a fixed custom location. If you set it, use the same value in the shell and service definition so every Porto command opens the same state.

Back up that directory while the daemon is stopped. Treat the certificate authority private key as sensitive.

