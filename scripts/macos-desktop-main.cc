// Portions Copyright (c) 2022 Slack Technologies, Inc.
// Governed by the MIT license in LICENSE.
// Preserve the non-helper bootstrap from Electron's shell/app/electron_main_mac.cc
// and uv_stdio_fix.cc, including the runAsNode fuse. Remove with the power guard.
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <sys/stat.h>

extern "C" int ElectronMain(int argc, char* argv[]);
extern "C" int ElectronInitializeICUandStartNode(int argc, char* argv[]);
namespace electron::fuses {
bool IsRunAsNodeEnabled();
}

int main(int argc, char* argv[]) {
    FILE* streams[] = {stdin, stdout, stderr};
    const char* modes[] = {"r", "w", "w"};
    for (int descriptor = 0; descriptor < 3; ++descriptor) {
        struct stat status;
        if (fstat(descriptor, &status) < 0 && errno == EBADF &&
            !freopen("/dev/null", modes[descriptor], streams[descriptor])) {
            perror("Porto: unable to restore a standard stream");
            return EXIT_FAILURE;
        }
    }

    const char* run_as_node = getenv("ELECTRON_RUN_AS_NODE");
    if (electron::fuses::IsRunAsNodeEnabled() && run_as_node && *run_as_node) {
        return ElectronInitializeICUandStartNode(argc, argv);
    }
    return ElectronMain(argc, argv);
}
