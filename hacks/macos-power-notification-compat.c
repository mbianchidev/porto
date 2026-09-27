/*
 * Temporary, opt-in workaround for the power-notification crash observed on macOS 27.
 * TODO(Electron): remove after the locked Electron ships and passes the upstream fix:
 * https://github.com/chromium/chromium/commit/69403d85b78bef2370cc9f8206dce84c5ff63ea4
 * https://issues.chromium.org/issues/562777834
 * The unguarded probe in macos-power-notification-compat.test.cjs is the removal gate.
 * Do not bundle this library into Windows/Linux or weaken macOS security to load it.
 */
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>

static pthread_once_t source_once = PTHREAD_ONCE_INIT;
static CFRunLoopSourceRef unavailable_source;

static void no_power_events(void *info) {
    (void)info;
}

static void create_unavailable_source(void) {
    CFRunLoopSourceContext context = { .version = 0, .perform = no_power_events };
    unavailable_source = CFRunLoopSourceCreate(kCFAllocatorDefault, 0, &context);
    if (!unavailable_source) {
        fputs("Porto: unable to allocate the power-notification fallback source.\n", stderr);
        abort();
    }
    fputs("Porto: macOS power notifications are unavailable. Continuing without sleep/wake events.\n", stderr);
}

static CFRunLoopSourceRef porto_notification_source(IONotificationPortRef port) {
    if (port) return IONotificationPortGetRunLoopSource(port);
    // Retain one inert source for the process lifetime, including run-loop removal.
    pthread_once(&source_once, create_unavailable_source);
    return unavailable_source;
}

__attribute__((constructor))
static void confine_compatibility_library(void) {
    if (unsetenv("DYLD_INSERT_LIBRARIES") != 0) {
        perror("Porto: unable to clear the child-process compatibility environment");
        abort();
    }
}

__attribute__((used, section("__DATA,__interpose")))
static struct {
    CFRunLoopSourceRef (*replacement)(IONotificationPortRef);
    CFRunLoopSourceRef (*original)(IONotificationPortRef);
} notification_source_interpose = {
    porto_notification_source,
    IONotificationPortGetRunLoopSource,
};
