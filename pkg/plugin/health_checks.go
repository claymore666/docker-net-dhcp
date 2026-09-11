// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// The three status values of draft-inadarei-api-health-check-06 section
// 3.1, which /Plugin.Health's `status` field and every entry of its
// `checks` object carry.
//
// WHAT IS ADOPTED AND WHAT IS NOT. The document SHAPE is the draft's:
// a `status` of pass/warn/fail, a `checks` object whose values are
// single-element arrays (section 4), and per-check `status`,
// `observedValue`, `observedUnit`, `time` and `output`. The TRANSPORT
// rules are not. Section 3.1 requires a 4xx-5xx response for `fail`;
// this endpoint answers 200 whatever the status, because the flag it
// sits beside LATCHES: one recovery failure an hour ago would make the
// socket answer 5xx for the life of the process, and everything that
// polls a plugin socket reads a non-2xx as "the plugin is down". The
// media type stays application/json for the same reason -- see the
// Observability section of docs/reference.md, which states both
// deviations for operators.
const (
	statusPass = "pass"
	statusWarn = "warn"
	statusFail = "fail"
)

// HealthCheck is one element of the `checks` object.
//
// Field names are the draft's, camelCase and all, rather than this
// repo's snake_case: the point of the shape is that a reader who knows
// the draft can read this document, and a renamed field is a shape that
// only looks like one.
type HealthCheck struct {
	Status        string `json:"status"`
	ObservedValue int64  `json:"observedValue"`
	ObservedUnit  string `json:"observedUnit"`
	// Time is when the counter behind this check LAST MOVED, in
	// RFC3339 with nanoseconds -- not when this response was built.
	// The flags latch, so without it `fail` cannot be read as anything
	// but "at some point during this process", and "faulted an hour
	// ago" and "faulting right now" are the same document. A counter
	// that has never moved carries the time of this reading, which is
	// the honest statement for a zero: nothing has been observed as of
	// now.
	Time string `json:"time"`
	// Output is omitted for a passing check, per section 4.8.
	Output string `json:"output,omitempty"`
}

// stampedCounter is a counter that remembers when it last moved.
//
// A SEPARATE `lastMoved` MAP WOULD BE A NEIGHBOUR, NOT A GUARD. The
// stamp has to be impossible to forget, and the way to make it
// impossible is to put it inside the thing being incremented: every
// existing `.Add(1)` site keeps its exact spelling and gains the
// timestamp, and a new call site cannot bump the value without moving
// the stamp. Only the counters that back a check carry one -- the rest
// are informational and nothing renders a time for them.
type stampedCounter struct {
	n  atomic.Int32
	at atomic.Int64
}

func (c *stampedCounter) Load() int32 { return c.n.Load() }

func (c *stampedCounter) Add(d int32) int32 {
	v := c.n.Add(d)
	if d != 0 {
		c.at.Store(time.Now().UnixNano())
	}
	return v
}

func (c *stampedCounter) Store(v int32) {
	c.n.Store(v)
	c.at.Store(time.Now().UnixNano())
}

// LastMoved is the zero Time when the counter has never moved.
func (c *stampedCounter) LastMoved() time.Time {
	ns := c.at.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// intCounter is what addUint64 and bumpFamily accept, so that a
// stamped counter and a plain atomic can both be passed to them.
// *atomic.Int32 satisfies it as it stands.
type intCounter interface {
	Load() int32
	Store(int32)
	Add(int32) int32
}

// laterOf is the movement time of a family pair: the aggregate moved
// when either half did.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// healthChecks builds the `checks` object and the top-level status.
//
// THE FAIL SET IS NOT A LIST HERE. It is read out of metricDefs'
// `healthy` declaration -- the same field scripts/check-health-contract.sh
// already reconciles against the reference table's healthy-affecting
// column and against the `Healthy` expression's term count. So `status:
// "pass"` beside `healthy: false` is not a thing that has to be tested
// for and remembered; it is a thing that would need two different
// readings of one declaration to happen at all. The warn set is
// declared on the same table, one axis over.
//
// stamps is keyed by the same json tag; a check whose field has no
// stamp renders as though the counter had never moved, which is why
// TestHealthChecks_EveryCheckHasAStamp exists.
func healthChecks(h HealthResponse, stamps map[string]time.Time, now time.Time) (string, map[string][]HealthCheck) {
	byTag := healthFieldsByTag(h)
	out := make(map[string][]HealthCheck, 16)
	worst := statusPass

	for _, d := range metricDefs() {
		sev := ""
		switch {
		case d.healthy:
			sev = statusFail
		case d.warn:
			sev = statusWarn
		default:
			continue
		}

		c := HealthCheck{Status: statusPass, ObservedUnit: d.unit}

		raw, ok := byTag[d.field]
		n, err := strconv.ParseInt(raw, 10, 64)
		if !ok || err != nil {
			// Unreachable while every check names an integer field,
			// which TestHealthChecks_EveryCheckFieldIsAnInteger holds.
			// Reported as a failing check rather than skipped: a check
			// that quietly disappears is the hole this document exists
			// to close.
			c.Status = statusFail
			c.Output = fmt.Sprintf("the health field %q is not a number this check can read", d.field)
			out[d.field] = []HealthCheck{c}
			worst = statusFail
			continue
		}

		c.ObservedValue = n
		at, seen := stamps[d.field]
		if seen && !at.IsZero() {
			c.Time = at.Format(time.RFC3339Nano)
		} else {
			c.Time = now.Format(time.RFC3339Nano)
		}
		if n > 0 {
			c.Status = sev
			c.Output = d.action
			if sev == statusFail || worst == statusPass {
				worst = sev
			}
		}
		out[d.field] = []HealthCheck{c}
	}

	return worst, out
}
