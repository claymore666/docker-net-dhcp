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

// The four arms of errNoSandboxKey, each its own sentinel.
//
// WHY THEY EXIST. errNoSandboxKey alone says the key route was refused
// and says nothing about WHY, and the why is the whole of SECURITY.md's
// causal claim: that the plugin's /var/run/docker bind mount is a
// snapshot, so the entry it opens is the ordinary file libnetwork
// created underneath. A refusal because the daemon runs with a
// non-default --exec-root, and therefore publishes keys under a
// directory this plugin does not accept, produces a byte-identical
// count on the single sentinel — same counters, same log line, a
// different cause and a different remedy.
//
// Wrapping one arm per refusal lets openSandboxNetNS count them
// separately, which is what makes the claim readable from outside the
// process instead of inferred from a green run.
//
// EXHAUSTIVE BY CONSTRUCTION: every errNoSandboxKey return below wraps
// exactly one of these, and the classifier's residual arm counts
// anything that reaches it another way — so the arms always sum to
// sandbox_key_entry_failures rather than nearly doing so.
var (
	// errSandboxKeyAbsent: there is no key at all, from either source
	// -- the Join request carried none and the container inspect
	// reported none either (dhcp_manager.go reads the second when the
	// first is empty, which is how recovery's synthesised JoinRequest
	// still reaches a real key).
	//
	// SPLIT OUT OF errSandboxKeyNotPermitted IN 2.0-alpha.1, because
	// the empty key took that arm silently and the two want opposite
	// readings: not-permitted is documented as the arm that is NOT
	// expected, whose remedy is a change to this plugin, and an absent
	// key is neither a host misconfiguration nor a plugin bug but the
	// absence of the input, with the PID route carrying the attach.
	// Not observed on any measured host -- the recovery cell requires
	// every arm to be zero on the recovered instance -- and published
	// for the same reason errSandboxKeyWrongNSType is.
	errSandboxKeyAbsent = errors.New("no sandbox key was published for this endpoint")

	// errSandboxKeyNotPermitted: the key is non-empty and does not name
	// an entry of one of sandboxNetnsDirs. A daemon with a non-default
	// --exec-root looks like this, and so does a malformed key.
	errSandboxKeyNotPermitted = errors.New("sandbox key is not an entry of a permitted directory")

	// errSandboxKeyNotANamespace: the entry opened and is not a
	// namespace at all. This is the placeholder file libnetwork
	// creates before it bind-mounts the namespace over it, seen
	// through a mount namespace the later bind never reached.
	errSandboxKeyNotANamespace = errors.New("sandbox key entry is not a namespace")

	// errSandboxKeyWrongNSType: a namespace, of some other type.
	// NS_GET_NSTYPE answers for any namespace, so this arm is what
	// separates "the ioctl worked" from "this is a network namespace".
	errSandboxKeyWrongNSType = errors.New("sandbox key entry is a namespace of the wrong type")
)

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

// countSandboxKeyRefusal attributes one key-route failure to exactly one
// arm.
//
// TOTAL BY CONSTRUCTION, and that is the property worth having rather
// than three counters that nearly account for the aggregate. Every
// caller bumps sandboxKeyEntryFailures and then calls this exactly once,
// and this always increments exactly one counter — so
//
//	sandbox_key_entry_failures ==
//	    sandbox_key_absent + sandbox_key_not_permitted +
//	    sandbox_key_not_a_namespace + sandbox_key_wrong_ns_type +
//	    sandbox_key_unavailable
//
// holds for every plugin instance, and a reader who finds it broken has
// found a bug rather than a rounding difference. A new refusal added
// upstream of this without its own sentinel lands in the residual arm,
// where it is visible, instead of silently making the sum wrong.
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
