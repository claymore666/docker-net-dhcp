package util

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// TestAwaitNetNS_DeadlineCarriesLastError pins the #317 diagnosability
// contract: when the await budget expires, the error must still satisfy
// errors.Is(_, context.DeadlineExceeded) for callers that branch on it,
// AND carry the last underlying open error. The production incident was
// a persistent EACCES (missing CAP_SYS_PTRACE) reported as a bare
// "context deadline exceeded" — indistinguishable from a startup race.
func TestAwaitNetNS_DeadlineCarriesLastError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := AwaitNetNS(ctx, "/nonexistent/netns/path", 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error for a path that never appears")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error must wrap context.DeadlineExceeded, got: %v", err)
	}
	if !strings.Contains(err.Error(), "last attempt:") ||
		!strings.Contains(err.Error(), "no such file") {
		t.Errorf("error must carry the last underlying cause (ENOENT here), got: %v", err)
	}
}

// TestAwaitLinkByIndex_DeadlineCarriesLastError is the link-await
// sibling of the test above.
func TestAwaitLinkByIndex_DeadlineCarriesLastError(t *testing.T) {
	handle, err := netlink.NewHandle()
	if err != nil {
		t.Skipf("cannot open netlink handle in this environment: %v", err)
	}
	defer handle.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// An index that cannot exist: kernel ifindexes are small positive
	// ints; 1<<30 will never appear during the 50ms budget.
	_, err = AwaitLinkByIndex(ctx, handle, 1<<30, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error for a link index that never appears")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error must wrap context.DeadlineExceeded, got: %v", err)
	}
	if !strings.Contains(err.Error(), "last attempt:") {
		t.Errorf("error must carry the last underlying cause, got: %v", err)
	}
}
