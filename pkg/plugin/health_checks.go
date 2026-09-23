// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// The status values of draft-inadarei-api-health-check-06 section 3.1; /Plugin.Health
// answers 200 on fail as well because its flags latch, as docs/reference.md states (#910).
const (
	statusPass = "pass"
	statusWarn = "warn"
	statusFail = "fail"
)

// HealthCheck is one element of the `checks` object.
type HealthCheck struct {
	Status        string `json:"status"`
	ObservedValue int64  `json:"observedValue"`
	ObservedUnit  string `json:"observedUnit"`
	// Time is when the counter behind this check last moved, not when the response was built.
	Time string `json:"time"`
	// Output is omitted for a passing check, per section 4.8.
	Output string `json:"output,omitempty"`
}

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

type intCounter interface {
	Load() int32
	Store(int32)
	Add(int32) int32
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// healthChecks reads the fail and warn sets from metricDefs, the declaration
// check-health-contract.sh reconciles with the reference (#910).
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
