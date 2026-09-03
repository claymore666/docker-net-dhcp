//go:build linux

package runtime

import (
	"syscall"
	"unsafe"

	"github.com/claymore666/dhcp-golib/proto"
)

// clockBoottime is CLOCK_BOOTTIME, spelled out because Go's syscall package
// does not export it. It lives in <linux/time.h> and has been 7 since 2.6.39.
const clockBoottime = 7

// monoNow reads CLOCK_BOOTTIME.
//
// A raw syscall rather than golang.org/x/sys/unix.ClockGettime: this library
// has no third-party dependencies and one clock reading is not worth the
// first one. If x/sys arrives for another reason, this should call into it.
//
// A failure returns 0. An error path here would put an error check on every
// deadline computation in the library, for a call whose failure means a
// missing vDSO or an unsupported clock id — a broken kernel, not a condition
// to recover from.
func monoNow() proto.Instant {
	var ts syscall.Timespec
	_, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME,
		uintptr(clockBoottime), uintptr(unsafe.Pointer(&ts)), 0)
	if errno != 0 {
		return 0
	}
	return proto.Instant(ts.Sec)*proto.Instant(proto.Second) + proto.Instant(ts.Nsec)
}
