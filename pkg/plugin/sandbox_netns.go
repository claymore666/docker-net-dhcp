// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// errNoSandboxKey covers a key not yet published and a shape this plugin refuses; only the caller's deadline tells
// them apart (#725).
var errNoSandboxKey = errors.New("no usable sandbox key")

// One sentinel per refusal arm, so a non-default --exec-root is counted apart from an unpropagated
// bind mount; the classifier's residual arm keeps the arms summing to the total (#725).
var (
	// errSandboxKeyAbsent: neither Join nor the container inspect carried a key, and the PID route carries the attach
	// (#725).
	errSandboxKeyAbsent = errors.New("no sandbox key was published for this endpoint")

	errSandboxKeyNotPermitted = errors.New("sandbox key is not an entry of a permitted directory")

	// errSandboxKeyNotANamespace: the placeholder file libnetwork creates before its bind mount, seen from a mount
	// namespace the bind never reached (#725).
	errSandboxKeyNotANamespace = errors.New("sandbox key entry is not a namespace")

	errSandboxKeyWrongNSType = errors.New("sandbox key entry is a namespace of the wrong type")
)

// openSandboxNetNSByKeyIn opens the sandbox netns libnetwork bind-mounts at the key; unlike /proc/<pid>/ns/net it
// needs no host PID namespace, no CAP_SYS_PTRACE and no recycled-PID check (#317, #688).
func openSandboxNetNSByKeyIn(dirs []string, sandboxKey string) (netns.NsHandle, error) {
	if sandboxKey == "" {
		return netns.None(), fmt.Errorf("%w (%w): this endpoint has no sandbox key, so the key route "+
			"was never attempted and the container PID route carries the attach",
			errNoSandboxKey, errSandboxKeyAbsent)
	}
	dir, name := splitSandboxKeyIn(dirs, sandboxKey)
	if dir == "" {
		return netns.None(), fmt.Errorf("%w (%w): %q is not an entry of %v",
			errNoSandboxKey, errSandboxKeyNotPermitted, sandboxKey, dirs)
	}

	dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return netns.None(), fmt.Errorf("open sandbox netns directory %s: %w", dir, err)
	}
	defer unix.Close(dirFd)

	// Relative to the directory fd, so the name cannot escape the directory it was validated against (#688).
	fd, err := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return netns.None(), fmt.Errorf("open sandbox netns %s/%s: %w", dir, name, err)
	}

	// An empty placeholder file opens cleanly and setns on it fails later with EINVAL; measured on the lane
	// 2026-09-04, every attach opened and every persistent client then died that way. NS_GET_NSTYPE fails with ENOTTY
	// on a regular file (#725).
	nsType, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil {
		unix.Close(fd)
		return netns.None(), fmt.Errorf("%w (%w): %s/%s opened, but it is not a namespace (%w) — the "+
			"daemon's sandbox mounts are not propagated into this plugin's mount namespace",
			errNoSandboxKey, errSandboxKeyNotANamespace, dir, name, err)
	}
	if nsType != unix.CLONE_NEWNET {
		unix.Close(fd)
		return netns.None(), fmt.Errorf("%w (%w): %s/%s is a namespace of type %#x, not a network namespace",
			errNoSandboxKey, errSandboxKeyWrongNSType, dir, name, nsType)
	}
	return netns.NsHandle(fd), nil
}

// awaitSandboxNetNSByKey retries until ctx ends; the deadline error carries the last cause (#317), and
// errNoSandboxKey is final at once (#401).
func awaitSandboxNetNSByKey(ctx context.Context, sandboxKey string, interval time.Duration) (netns.NsHandle, error) {
	return awaitSandboxNetNSByKeyIn(ctx, sandboxNetnsDirs, sandboxKey, interval)
}

func awaitSandboxNetNSByKeyIn(ctx context.Context, dirs []string, sandboxKey string, interval time.Duration) (netns.NsHandle, error) {
	var lastErr error
	for {
		ns, err := openSandboxNetNSByKeyIn(dirs, sandboxKey)
		if err == nil {
			return ns, nil
		}
		if errors.Is(err, errNoSandboxKey) {
			return netns.None(), err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return netns.None(), fmt.Errorf("%w (last attempt: %w)", ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

// countSandboxKeyRefusal increments exactly one arm, so the arms sum to sandbox_key_entry_failures (#725).
func (p *Plugin) countSandboxKeyRefusal(err error) {
	switch {
	case errors.Is(err, errSandboxKeyAbsent):
		p.sandboxKeyAbsent.Add(1)
	case errors.Is(err, errSandboxKeyNotPermitted):
		p.sandboxKeyNotPermitted.Add(1)
	case errors.Is(err, errSandboxKeyNotANamespace):
		p.sandboxKeyNotANamespace.Add(1)
	case errors.Is(err, errSandboxKeyWrongNSType):
		p.sandboxKeyWrongNSType.Add(1)
	default:
		p.sandboxKeyUnavailable.Add(1)
	}
}
