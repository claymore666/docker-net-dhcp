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

// errNoSandboxKey is returned when the container has no sandbox key to
// enter through. It is a distinct sentinel because the two ways of
// having no key want different readings: Docker has not published one
// yet (retry), or it published a shape this plugin does not accept
// (refuse). Both arrive here; only the caller's deadline tells them
// apart, so the error text names the key it rejected.
var errNoSandboxKey = errors.New("no usable sandbox key")

// openSandboxNetNSByKeyIn opens the network namespace libnetwork
// bind-mounts for a sandbox, named by the sandbox key Docker hands to
// Join and reports from ContainerInspect.
//
// WHY THIS AND NOT /proc/<pid>/ns/net. The PID route needs the host PID
// namespace to see the container's task at all, needs CAP_SYS_PTRACE to
// open ns/net when the container's init runs as a non-root user, and
// needs an identity check on every open because a PID that has been
// recycled inside the attach window names an arbitrary host task (#317,
// #688). None of that applies here: the key names a bind mount of the
// sandbox's own netns, created by the daemon and unlinked when the
// sandbox goes away, so there is no second thing it can name and no
// window in which it comes to name one.
//
// The key is validated against sandboxNetnsDirs rather than opened as
// given. An unrecognised shape is refused, not guessed at: the
// alternative is opening a path the daemon did not choose, and
// splitSandboxKeyIn is already the tree's answer to that question for
// sandboxGone.
//
// The returned handle is an open file descriptor. It is the caller's to
// close.
// dirs is a parameter so the refusal table can be driven without root,
// exactly as splitSandboxKeyIn takes it; production passes
// sandboxNetnsDirs through awaitSandboxNetNSByKey and there is no
// second caller, which is why no zero-argument wrapper exists to go
// stale beside it.
func openSandboxNetNSByKeyIn(dirs []string, sandboxKey string) (netns.NsHandle, error) {
	dir, name := splitSandboxKeyIn(dirs, sandboxKey)
	if dir == "" {
		return netns.None(), fmt.Errorf("%w: %q is not an entry of %v", errNoSandboxKey, sandboxKey, dirs)
	}

	dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return netns.None(), fmt.Errorf("open sandbox netns directory %s: %w", dir, err)
	}
	defer unix.Close(dirFd)

	// Relative to the directory fd, so the name cannot escape the
	// directory it was validated against between the check and the
	// open. Same rule openContainerProc follows for /proc.
	fd, err := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return netns.None(), fmt.Errorf("open sandbox netns %s/%s: %w", dir, name, err)
	}

	// A SUCCESSFUL OPEN IS NOT AN ENTRY, and telling the two apart is
	// the whole reason this check exists rather than a comment saying
	// it should.
	//
	// libnetwork creates each sandbox entry as an ordinary empty file
	// and then bind-mounts the namespace over it. Whether the plugin
	// sees the file or the namespace is a property of MOUNT
	// PROPAGATION into the plugin's own mount namespace, not of the
	// path — and the plugin's /var/run/docker mount does not
	// necessarily carry submounts the daemon makes after it was
	// established. Opening the empty file underneath succeeds; setns
	// on the result fails with EINVAL much later, deep inside a
	// netlink call, on a code path that reads as a plugin fault.
	// MEASURED on the lane 2026-09-04: every attach opened cleanly and
	// every persistent client then died with "failed to set into
	// network namespace ... invalid argument".
	//
	// NS_GET_NSTYPE answers the only question that matters — is this
	// descriptor a NETWORK namespace — and answers it here, where the
	// caller can still fall back, instead of two layers down where it
	// cannot. On a regular file the ioctl fails with ENOTTY.
	nsType, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil {
		unix.Close(fd)
		return netns.None(), fmt.Errorf("%w: %s/%s opened, but it is not a namespace (%w) — the "+
			"daemon's sandbox mounts are not propagated into this plugin's mount namespace",
			errNoSandboxKey, dir, name, err)
	}
	if nsType != unix.CLONE_NEWNET {
		unix.Close(fd)
		return netns.None(), fmt.Errorf("%w: %s/%s is a namespace of type %#x, not a network namespace",
			errNoSandboxKey, dir, name, nsType)
	}
	return netns.NsHandle(fd), nil
}

// awaitSandboxNetNSByKey polls openSandboxNetNSByKeyIn until it succeeds,
// ctx is cancelled, or interval-paced retries exhaust.
//
// A key that is absent right now is the ordinary case at Join: the
// request arrives while the daemon is still assembling the sandbox. It
// is also what a container that has just gone away looks like, and the
// two are separated by the deadline rather than by the first attempt —
// so, like the other Await helpers, the deadline error carries the last
// attempt's cause. Losing it is what made a refusal and a slow start
// read identically (#317).
//
// errNoSandboxKey is NOT retried. A key that is empty, or that names a
// directory this plugin does not accept, is refused permanently for
// this (key) pair — polling it can only ever end in the deadline, and
// spending the whole attach budget on an answer that was final at the
// first attempt is what #401 removed from the sibling helpers. It is
// also the difference between reaching the PID fallback at once and
// reaching it with no budget left to use it.
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
