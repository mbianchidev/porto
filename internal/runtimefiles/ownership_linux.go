//go:build linux

package runtimefiles

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func resolveOwnership(descriptor Descriptor) (Descriptor, error) {
	connection, err := net.Dial("unix", descriptor.Address)
	if err != nil {
		return descriptor, err
	}
	defer connection.Close()
	socket, ok := connection.(*net.UnixConn)
	if !ok {
		return descriptor, errors.New("runtime ownership probe is not a Unix socket")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return descriptor, err
	}
	var peer *unix.Ucred
	var probeErr error
	if err := raw.Control(func(fd uintptr) { peer, probeErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return descriptor, err
	}
	if probeErr != nil {
		return descriptor, probeErr
	}
	descriptor.RuntimePID = peer.Pid
	selfMount, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return descriptor, err
	}
	peerMount, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", peer.Pid))
	if err != nil {
		return descriptor, err
	}
	if selfMount == peerMount {
		descriptor.RuntimePID = 0
	}
	selfUser, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return descriptor, err
	}
	peerUser, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", peer.Pid))
	if err != nil {
		return descriptor, err
	}
	if selfUser == peerUser {
		descriptor.UIDMap, descriptor.GIDMap = nil, nil
		return descriptor, nil
	}
	descriptor.UIDMap, err = readIDMap(fmt.Sprintf("/proc/%d/uid_map", peer.Pid))
	if err != nil {
		return descriptor, err
	}
	descriptor.GIDMap, err = readIDMap(fmt.Sprintf("/proc/%d/gid_map", peer.Pid))
	return descriptor, err
}

func readIDMap(name string) ([]IDMap, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	mappings := make([]IDMap, 0)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, errors.New("invalid runtime user-namespace map")
		}
		values := make([]int64, 3)
		for index, field := range fields {
			values[index], err = strconv.ParseInt(field, 10, 64)
			if err != nil || values[index] < 0 {
				return nil, errors.New("invalid runtime user-namespace identity range")
			}
		}
		if values[2] == 0 {
			return nil, errors.New("empty runtime user-namespace identity range")
		}
		mappings = append(mappings, IDMap{Namespace: values[0], Host: values[1], Size: values[2]})
	}
	if len(mappings) == 0 {
		return nil, errors.New("runtime user-namespace map is empty")
	}
	return mappings, scanner.Err()
}
