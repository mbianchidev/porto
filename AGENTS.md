# Temporary macOS 27 Electron workaround

When changing Electron dependencies, inspect the temporary guard in
`hacks/macos-power-notification-compat.c`, its recovery launcher
`hacks/open-porto-macos-27.sh`, and the packaged bootstrap in
`scripts/macos-desktop-bootstrap.cjs` and `scripts/macos-desktop-main.cc`.
They work around a failed `IORegisterForSystemPower` call followed by a
null-port dereference, observed in release v1.2.12 on macOS 27.
Electron 44.4.5 still fails the injected-failure probe without the guard.

macOS packages link the guard into their main executable through a bundled
dylib, before Electron starts; no user command or loader environment is needed.
Preserve Electron's fuse-aware Node dispatch and closed-stdio repair when
changing the native entry point. Perform native edits before code signing.
Build both macOS desktop archives and DMGs on macOS; fail rather than publish
an unguarded cross-built package. Never add the guard to Windows/Linux
packages, propagate it to helper processes, or enable it system-wide.

Source development also prepares a guarded, cached `Porto Dev.app` through
`ui/electron/dev-launcher.cjs` and `scripts/macos-development-app.cjs`. Keep its
embedded source entry and Electron executable name so reopening does not lose
the app path and development does not become packaged mode. Retire its native
guard hook with the package guard, while retaining source-reopen regression
coverage; never modify the npm-installed Electron runtime.

The verified upstream fix is [Chromium 69403d85](https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4)
([issue 562777834](https://issues.chromium.org/issues/562777834)); no matching
Electron issue was found when this workaround was added.

Run `node --test hacks/macos-power-notification-compat.test.cjs` on macOS after
upgrading Electron. Its unguarded, injected-failure probe deliberately fails
with removal instructions once Electron no longer needs the guard. Verify the
fixed locked Electron version, remove both helper files, the native bootstrap
and its packaging hooks, and their recovery documentation. Retain the
injected-failure scenario as a passing unguarded regression test, including
packaged-app rendering and clean shutdown. Do not disable the probe or weaken
its assertions to retain an obsolete workaround. Update this instruction when
the workaround is removed.
