# Temporary macOS 27 Electron workaround

When changing Electron dependencies, inspect the opt-in recovery helper in
`hacks/macos-power-notification-compat.c` and `hacks/open-porto-macos-27.sh`.
It works around a failed `IORegisterForSystemPower` call followed by a null-port
dereference, observed on macOS 27. It must not enter Windows/Linux packages or
be enabled globally.

The verified upstream fix is [Chromium 69403d85](https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4)
([issue 562777834](https://issues.chromium.org/issues/562777834)); no matching
Electron issue was found when this workaround was added.

Run `node --test hacks/macos-power-notification-compat.test.cjs` on macOS after
upgrading Electron. Its unguarded, injected-failure probe deliberately fails
with removal instructions once Electron no longer needs the guard. Verify the
fixed locked Electron version, remove both helper files and their recovery
documentation, and retain the injected-failure scenario as a passing unguarded
regression test. Do not disable the probe or weaken its assertions to retain an
obsolete workaround. Update this instruction when the workaround is removed.
