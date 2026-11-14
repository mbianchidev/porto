# Using the Porto CLI

Use the [Porto agent skill](.github/skills/porto/SKILL.md) whenever a task needs
Porto installed or asks to run projects, containers, Compose stacks, Kubernetes
clusters or VMs through `porto`. Check `porto version` before installing, and ask
before destructive or global changes. Users can hand the same workflow to an
agent in another project with this prompt:

```text
Read https://raw.githubusercontent.com/mbianchidev/porto/main/.github/skills/porto/SKILL.md and follow it. Install Porto if `porto version` fails, make sure its daemon is running, then use the porto CLI for this task. Ask me before deleting anything or changing global Docker, HTTPS or Gatekeeper settings.
```

Keep the skill in sync with `porto help` (`cmd/porto/main.go`) and the docs when
CLI commands, flags, installer variables or safety behavior change.

# macOS startup regression coverage

Electron 44.9.3 passes the unguarded, injected power-registration failure that
previously required Porto's temporary macOS 27 workaround. The upstream fix is
[Chromium 69403d85](https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4)
([issue 562777834](https://issues.chromium.org/issues/562777834)).

Run `node --test scripts/macos-startup.test.cjs` on macOS after upgrading Electron.
The synthetic failure injector must still exercise `IORegisterForSystemPower`,
and the unmodified runtime, cached source app, and packaged app must render and
exit cleanly without a native guard. Do not restore expected crashes or weaken
the injected-failure, rendering, executable-path, Node-mode, or closed-stdio
assertions. Confine the test-only loader environment to the parent process.

Source development retains a cached `Porto Dev.app` through
`ui/electron/dev-launcher.cjs` and `scripts/macos-development-app.cjs`.
Keep its embedded source entry and Electron executable name so argument-free
reopening resolves the source checkout and development stays unpackaged.
Never patch the npm-installed Electron runtime.

Packages use Electron's original fuse-aware native entry point and standard
stream repair. Make package resource changes before signing. Build macOS
desktop archives and DMGs on macOS with the documented Icon Composer toolchain.
