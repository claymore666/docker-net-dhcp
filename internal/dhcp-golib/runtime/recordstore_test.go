package runtime

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

func storeFixtureEvents(t *testing.T) []lease.RecordEvent {
	t.Helper()
	p := proto.DefaultParams([]byte{0x02, 0, 0, 0, 0, 1})
	p.ClientID = []byte{0xff, 0xde, 0xad, 0xbe, 0xef}
	p.Hostname = "fixture"
	p.Servers.Allow = []netip.Addr{netip.MustParseAddr("192.168.99.1")}
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	l := lease.Lease{
		Addr:         netip.MustParsePrefix("192.168.99.100/24"),
		Gateway:      netip.MustParseAddr("192.168.99.1"),
		DNS:          []netip.Addr{netip.MustParseAddr("192.168.99.1")},
		Domain:       "dhcp.test",
		MTU:          1400,
		ServerID:     netip.MustParseAddr("192.168.99.1"),
		Routes:       []wire.Route{{Dest: netip.MustParsePrefix("10.0.0.0/8"), Router: netip.MustParseAddr("192.168.99.1")}},
		DomainSearch: []string{"dhcp.test"},
		Acquired:     at,
		Renew:        at.Add(time.Minute),
		Rebind:       at.Add(105 * time.Second),
		Expire:       at.Add(2 * time.Minute),
		Options:      wire.Options{wire.OptionCode(53): {5}, wire.OptionCode(51): {0, 0, 0, 120}},
	}
	return []lease.RecordEvent{
		{ID: "rec-1", Seq: 1, At: at, Instance: "p1", Op: lease.OpCreate, Scope: "net-a",
			Family: lease.FamilyV4, CHAddr: []byte{0x02, 0, 0, 0, 0, 1}, Identity: []byte{0xff, 0xde, 0xad, 0xbe, 0xef}, Params: &p},
		{ID: "rec-1", Seq: 2, At: at, Instance: "p1", Op: lease.OpBind},
		{ID: "rec-1", Seq: 3, At: at, Instance: "p1", Op: lease.OpLease, Kind: lease.Acquired, Lease: &l},
		{ID: "rec-1", Seq: 4, At: at, Instance: "p1", Manager: "mgr-1", Op: lease.OpStats, Stats: &lease.Stats{Sent: 2, Received: 2, Steps: 9}},
		{ID: "rec-1", Seq: 5, At: at, Instance: "p1", Op: lease.OpLost, Reason: proto.ReasonStopped},
	}
}

// TestARecordEventSurvivesTheJSONLRoundTrip. The durable form is the only thing
// that outlives the process, so a field that does not encode is a field that
// does not exist after a restart — silently, because the record still folds.
//
// The comparison is the whole event, not a field list: an enumeration here
// would be an unrun checklist that a new field never joins.
func TestARecordEventSurvivesTheJSONLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	want := storeFixtureEvents(t)
	for _, ev := range want {
		if err := s.Append(ev); err != nil {
			t.Fatalf("Append(%s): %v", ev.Op, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reopened.Damage().Any() {
		t.Fatalf("a file nothing damaged reports %s", reopened.Damage())
	}
	if len(got) != len(want) {
		t.Fatalf("%d event(s) came back, %d went in", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("event %d did not survive the file.\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// And the fold over the reloaded events is the fold over the originals.
	a, b := lease.Rebuild(want), lease.Rebuild(got)
	if !reflect.DeepEqual(a.Records, b.Records) {
		t.Fatalf("the reloaded journal folds to a different record.\n got %+v\nwant %+v", b.Records, a.Records)
	}
	if len(a.Records) != 1 || !a.Records[0].Held {
		t.Fatalf("the fixture folds to %+v; it is meant to end holding a lease", a.Records)
	}
}

// TestLoadAnswersInAppendOrder is defeat row M-9. Replay-in-order is the whole
// value of an append-only log, and a Load that sorted or de-duplicated would
// satisfy any test that only counted lines.
func TestLoadAnswersInAppendOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Sequence numbers DESCENDING and record ids interleaved, so append order
	// is not recoverable by sorting on anything in the events.
	var want []uint64
	for i := uint64(9); i >= 1; i-- {
		id := "rec-1"
		if i%2 == 0 {
			id = "rec-2"
		}
		if err := s.Append(lease.RecordEvent{ID: id, Seq: i, Op: lease.OpBind}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		want = append(want, i)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d event(s) back, %d in", len(got), len(want))
	}
	for i := range want {
		if got[i].Seq != want[i] {
			t.Fatalf("position %d holds seq %d, was appended with %d", i, got[i].Seq, want[i])
		}
	}
}

// TestATornLineIsSkippedAndCounted is defeat row M-4.
//
// A process killed inside Append leaves a fragment at the tail. Refusing the
// whole file for it loses every record written before the crash; skipping it
// silently loses one record with no trace. Both are wrong, so it is skipped and
// COUNTED, and a torn tail is counted apart from an unreadable interior line
// because they mean different things — a crash, and two writers or a damaged
// file.
//
// The truncation is driven at every offset inside the last line rather than at
// one chosen point: a single offset measures one parse, not the property.
func TestATornLineIsSkippedAndCounted(t *testing.T) {
	whole := func(t *testing.T) []byte {
		t.Helper()
		path := filepath.Join(t.TempDir(), "records.jsonl")
		s, err := OpenRecordStore(path)
		if err != nil {
			t.Fatalf("OpenRecordStore: %v", err)
		}
		for _, ev := range storeFixtureEvents(t) {
			if err := s.Append(ev); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading back: %v", err)
		}
		return b
	}(t)

	full, damage := parseRecordLines(whole)
	if damage.Any() || len(full) != 5 {
		t.Fatalf("the undamaged fixture parsed as %d event(s) with %s; the control is broken", len(full), damage)
	}

	lastLine := 0
	for i := 0; i < len(whole)-1; i++ {
		if whole[i] == '\n' {
			lastLine = i + 1
		}
	}
	if lastLine == 0 {
		t.Fatal("the fixture is one line, so there is nothing before the tear to preserve")
	}

	tried, keptFour := 0, 0
	for cut := lastLine + 1; cut < len(whole); cut++ {
		tried++
		evs, damage := parseRecordLines(whole[:cut])
		if len(evs) < 4 {
			t.Fatalf("a tear at offset %d lost a record written before it: %d survived", cut, len(evs))
		}
		switch len(evs) {
		case 4:
			keptFour++
			if damage.TornTail != 1 {
				t.Fatalf("a tear at offset %d dropped a line and reported %s", cut, damage)
			}
		case 5:
			// The tear landed exactly after the object's closing brace: the
			// event is whole and only its newline is missing.
			if damage.Any() {
				t.Fatalf("a tear at offset %d kept the event and still reported %s", cut, damage)
			}
		}
		if damage.Skipped != 0 {
			t.Fatalf("a tear at offset %d was accounted as an interior line: %s", cut, damage)
		}
	}
	if tried == 0 || keptFour == 0 {
		t.Fatalf("%d offset(s) tried, %d of them torn; a table with no torn row measures nothing", tried, keptFour)
	}
	t.Logf("%d truncation offsets inside the last line, %d of them unreadable", tried, keptFour)
}

// TestAnUnreadableInteriorLineIsCountedApart. It cannot be a crash — the writer
// had already written the newline after it — so it is two writers or a damaged
// file, and it is a different number.
func TestAnUnreadableInteriorLineIsCountedApart(t *testing.T) {
	good, err := json.Marshal(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Scope: "net-a"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	file := append(append([]byte(nil), good...), []byte("\n{\"id\":\"rec-1\",\"op\":\nnot json at all\n")...)
	file = append(file, good...)
	file = append(file, '\n')

	evs, damage := parseRecordLines(file)
	if damage.Skipped != 2 || damage.TornTail != 0 {
		t.Fatalf("damage = %s, want 2 skipped and no torn tail: a broken line in the middle is not a crash", damage)
	}
	if len(evs) != 2 {
		t.Fatalf("%d event(s) survived a damaged interior line, want the 2 good ones", len(evs))
	}
	if _, empty := parseRecordLines(nil); empty.Any() {
		t.Fatalf("an empty file reported damage: %s", empty)
	}
	if evs, blank := parseRecordLines([]byte("\n\n")); len(evs) != 0 || blank.Any() {
		t.Fatalf("blank lines were counted as damage: %d event(s), %s", len(evs), blank)
	}

	// THE LAST LINE OF A COMPLETE FILE. A broken line in the final position of
	// a file that ends in a newline is still not a crash: the writer got the
	// newline out after it. Only the absence of that newline makes a fragment,
	// and a check that keys on the position alone cannot tell the two apart.
	closed := append(append([]byte(nil), good...), '\n')
	closed = append(closed, []byte("{\"id\":\"rec-1\",\n")...)
	evs, damage = parseRecordLines(closed)
	if damage.Skipped != 1 || damage.TornTail != 0 {
		t.Fatalf("damage = %s on a broken LAST line of a newline-terminated file, want 1 skipped and no torn tail", damage)
	}
	if len(evs) != 1 {
		t.Fatalf("%d event(s) survived, want the 1 good one", len(evs))
	}

	// And the same bytes WITHOUT that final newline are the fragment.
	evs, damage = parseRecordLines(closed[:len(closed)-1])
	if damage.TornTail != 1 || damage.Skipped != 0 {
		t.Fatalf("damage = %s on the same file with its last newline missing, want 1 torn tail and nothing skipped", damage)
	}
	if len(evs) != 1 {
		t.Fatalf("%d event(s) survived the torn tail, want the 1 good one", len(evs))
	}
}

// TestTwoWritersAppendWholeLines is note row D-9: an old plugin process and a
// new one appending to one file during an upgrade.
//
// O_APPEND plus one write per event is what makes their lines interleave as
// lines rather than as one corrupted one. Two stores over one path here are the
// same shape as two processes: the kernel serialises the offset and the write
// together, and nothing in this package holds a lock the other would see.
func TestTwoWritersAppendWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	const perWriter = 200

	var wg sync.WaitGroup
	for _, who := range []string{"old-process", "new-process"} {
		s, err := OpenRecordStore(path)
		if err != nil {
			t.Fatalf("OpenRecordStore: %v", err)
		}
		defer func() { _ = s.Close() }()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= perWriter; i++ {
				ev := lease.RecordEvent{ID: who, Seq: uint64(i), Op: lease.OpBind, Instance: who, Note: who + " padding padding padding padding padding"}
				if err := s.Append(ev); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	reader, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reader.Close() }()
	evs, err := reader.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d := reader.Damage(); d.Any() {
		t.Fatalf("two writers on one file produced an unreadable line: %s", d)
	}
	if len(evs) != 2*perWriter {
		t.Fatalf("%d line(s) readable, want %d", len(evs), 2*perWriter)
	}
	seen := map[string]uint64{}
	for _, ev := range evs {
		if ev.Seq != seen[ev.Instance]+1 {
			t.Fatalf("%s wrote seq %d after %d; its own lines are out of order", ev.Instance, ev.Seq, seen[ev.Instance])
		}
		seen[ev.Instance] = ev.Seq
	}
	if len(seen) != 2 {
		t.Fatalf("%d writer(s) are visible in the file, want 2", len(seen))
	}
}

// TestTheSyncPolicyIsClassifiedForEveryOperation walks lease.AllOps, so an
// operation added later cannot arrive with no answer to "is this line worth an
// fsync". The rule is in durableWrite's doc: what cannot be reconstructed is
// synced, what a later exchange can re-derive is not.
func TestTheSyncPolicyIsClassifiedForEveryOperation(t *testing.T) {
	want := map[lease.RecordOp]bool{
		lease.OpReserve: true,
		lease.OpCreate:  true,
		lease.OpRebind:  true,
		lease.OpAdopt:   true,
		lease.OpBind:    true,
		lease.OpLease:   true, // only the first lease of an instance; driven below
		lease.OpLost:    false,
		lease.OpLeave:   false,
		lease.OpRetain:  false,
		lease.OpClose:   false,
		lease.OpStats:   false,
		lease.OpExtra:   false,
	}
	ops := lease.AllOps()
	if len(ops) != len(want) {
		t.Fatalf("%d operation(s) exist and %d are classified here", len(ops), len(want))
	}
	for _, op := range ops {
		w, ok := want[op]
		if !ok {
			t.Fatalf("operation %s has no sync classification", op)
		}
		ev := lease.RecordEvent{ID: "rec-1", Op: op}
		if op == lease.OpLease {
			ev.Kind = lease.Acquired
		}
		if got := durableWrite(ev); got != w {
			t.Errorf("%s is synced = %v, want %v", op, got, w)
		}
	}
	if durableWrite(lease.RecordEvent{Op: lease.OpLease, Kind: lease.Renewed}) {
		t.Error("a renewal is synced; a lost renewal line costs an earlier INIT-REBOOT, which is the safe direction")
	}
	if durableWrite(lease.RecordEvent{Op: lease.OpLease, Kind: lease.Changed}) {
		t.Error("a changed lease is synced")
	}
}

// TestAppendRefusesAnEventItCannotWriteAsOneLine. One event is one line, and a
// value that encoded a newline would split it into two — the second of which
// would be an unreadable interior line at every future Load.
func TestAppendRefusesAnEventItCannotWriteAsOneLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Note: "one\ntwo"}); err != nil {
		t.Fatalf("a newline inside a string must be ESCAPED by the encoder, not refused: %v", err)
	}
	evs, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 1 || evs[0].Note != "one\ntwo" {
		t.Fatalf("an escaped newline did not survive: %+v", evs)
	}
	if s.Damage().Any() {
		t.Fatalf("damage after a note with a newline: %s", s.Damage())
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 2, Op: lease.OpBind}); err == nil {
		t.Fatal("appending to a closed store was accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close returned %v", err)
	}
}

// TestTheSyncPolicyIsAppliedToEveryAppend is the CALL SITE of durableWrite.
//
// TestTheSyncPolicyIsClassifiedForEveryOperation pins the policy as a
// function, which a call site wired to a constant satisfies just as well. An
// fsync leaves no trace a same-process test can read, so the only way to see
// which appends took one is to stand in for it.
func TestTheSyncPolicyIsAppliedToEveryAppend(t *testing.T) {
	var synced []string
	failNext := false
	real := syncRecordFile
	syncRecordFile = func(f *os.File) error {
		if failNext {
			return os.ErrClosed
		}
		synced = append(synced, f.Name())
		return real(f)
	}
	t.Cleanup(func() { syncRecordFile = real })

	type appended struct {
		ev   lease.RecordEvent
		want bool
	}
	var plan []appended
	seq := uint64(0)
	add := func(op lease.RecordOp, kind lease.EventKind) {
		seq++
		ev := lease.RecordEvent{ID: "rec-1", Seq: seq, Op: op, Kind: kind}
		plan = append(plan, appended{ev: ev, want: durableWrite(ev)})
	}
	for _, op := range lease.AllOps() {
		add(op, 0)
	}
	// OpLease is the one op whose answer depends on the event it carries, so
	// every kind of it is appended, not just the zero one.
	for _, k := range []lease.EventKind{lease.Acquired, lease.Renewed, lease.Failed, lease.Lost} {
		add(lease.OpLease, k)
	}

	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	wantSyncs := 0
	for _, p := range plan {
		before := len(synced)
		if err := s.Append(p.ev); err != nil {
			t.Fatalf("Append(%s/%s): %v", p.ev.Op, p.ev.Kind, err)
		}
		got := len(synced) > before
		if got != p.want {
			t.Errorf("%s/%s: synced = %v, durableWrite says %v", p.ev.Op, p.ev.Kind, got, p.want)
		}
		if p.want {
			wantSyncs++
		}
	}
	if wantSyncs == 0 || wantSyncs == len(plan) {
		t.Fatalf("%d of %d appends were durable: a policy that answers the same for everything makes this test blind",
			wantSyncs, len(plan))
	}

	// A failed fsync is a failed Append: a caller told the line is on disk
	// when it is not is the whole reason the policy exists.
	failNext = true
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: seq + 1, Op: lease.OpCreate}); err == nil {
		t.Error("Append returned nil after its fsync failed")
	}
}

// TestNoFieldOfARecordEventCanEncodeToARawNewline is the reason Append's
// newline check has never fired: encoding/json escapes a newline wherever one
// can appear. The check stays, because the failure it guards — one event split
// across two lines, every later line unreadable — is not recoverable, and the
// property it rests on belongs to a package this one does not own.
func TestNoFieldOfARecordEventCanEncodeToARawNewline(t *testing.T) {
	const nl = "one\ntwo"
	ev := lease.RecordEvent{
		ID: nl, Op: lease.OpCreate, Seq: 1,
		Instance: nl, Manager: nl, Scope: nl, StepsRef: nl, Note: nl,
		CHAddr: []byte(nl), Identity: []byte(nl),
		Extra: map[string]uint64{nl: 1},
	}
	p := proto.DefaultParams([]byte(nl))
	p.Hostname = nl
	p.ClientID = []byte(nl)
	ev.Params = &p

	// Every string-shaped field of the event is set, so a field added later
	// without a value here is a gap this test can name.
	rv := reflect.ValueOf(ev)
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rv.Field(i)
		switch f.Kind() {
		case reflect.String:
			if f.String() == "" {
				t.Errorf("%s is a string field this test leaves empty, so it drives no newline through it", rt.Field(i).Name)
			}
		case reflect.Slice:
			if f.Type().Elem().Kind() == reflect.Uint8 && f.Len() == 0 {
				t.Errorf("%s is a byte field this test leaves empty", rt.Field(i).Name)
			}
		}
	}

	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		t.Fatalf("the encoded event carries a raw newline at offset %d: %s", i, line)
	}
	var back lease.RecordEvent
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("the escaped form does not round trip: %v", err)
	}
	if back.Note != nl || string(back.CHAddr) != nl {
		t.Fatalf("the newline did not survive escaping: note %q, chaddr %q", back.Note, back.CHAddr)
	}
}

// TestANewStoresDirectoryIsMadeDurable is the other fsync, and it is not the
// one Append does.
//
// Syncing a file makes its CONTENTS durable and says nothing about the
// directory entry that names it. A machine that lost power right after the
// first creating event could otherwise come back with that event synced and no
// file to find it in — losing exactly the line the sync policy says nothing can
// re-derive.
func TestANewStoresDirectoryIsMadeDurable(t *testing.T) {
	var synced []string
	failDirs := false
	real := syncRecordFile
	syncRecordFile = func(f *os.File) error {
		st, err := f.Stat()
		if err == nil && st.IsDir() && failDirs {
			return os.ErrPermission
		}
		synced = append(synced, f.Name())
		return real(f)
	}
	t.Cleanup(func() { syncRecordFile = real })

	dir := t.TempDir()
	path := filepath.Join(dir, "records.jsonl")

	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore on a fresh file: %v", err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("creating the store synced %v, want exactly the containing directory %s", synced, dir)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Opening one that already exists syncs NOTHING: the entry is already
	// durable, and paying for an fsync on every process start would be a cost
	// with no failure behind it.
	synced = nil
	s, err = OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore on an existing file: %v", err)
	}
	if len(synced) != 0 {
		t.Errorf("re-opening an existing store synced %v, want nothing", synced)
	}
	// It is the same file, not a truncated one: a create-detection that opened
	// with O_TRUNC would pass every assertion above and lose the journal.
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, third time: %v", err)
	}
	defer func() { _ = s.Close() }()
	evs, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("%d event(s) survived re-opening the store, want 1", len(evs))
	}

	// A directory that cannot be synced fails the OPEN. A store that reported
	// success here would have promised a durability it did not get.
	failDirs = true
	other := filepath.Join(t.TempDir(), "records.jsonl")
	if got, err := OpenRecordStore(other); err == nil {
		_ = got.Close()
		t.Error("OpenRecordStore succeeded although the directory sync failed")
	}
}

// TestTheDamageCountIsReadableThroughThePort is finding 5 of review round 1.
//
// A caller holding a lease.Store could be handed "every event" by a store that
// had just dropped one and, with Append and Load alone, had no way to ask. The
// count is on the PORT for that reason, and this test reads it there — through
// the interface, not through *RecordStore — so an implementation that answered
// only on its concrete type would not satisfy it.
func TestTheDamageCountIsReadableThroughThePort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	good, err := json.Marshal(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	file := append(append([]byte(nil), good...), '\n')
	file = append(file, []byte("{\"id\":\"rec-1\",\nnot json\n")...)
	file = append(file, good...)
	file = append(file, '\n')
	file = append(file, []byte("{\"id\":")...) // a fragment, no newline
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rs, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = rs.Close() }()

	var store lease.Store = rs
	// Opening the file had to REPAIR it, so the count is readable before any
	// Load — and it is the whole file's count, not the repair's own tally,
	// because a repairing open derives it with the parse Load uses. Three: the
	// two broken interior lines and the terminated fragment.
	if d := store.Damage(); d.Skipped != 3 || d.TornTail != 0 {
		t.Errorf("damage before any Load = %s, want 3 skipped — what the Load below reports", d)
	}
	evs, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("%d event(s) read, want the 2 good ones", len(evs))
	}
	d := store.Damage()
	if d.TornTail != 0 || d.Skipped != 3 {
		t.Fatalf("damage through the port = %s, want 3 skipped: the two interior lines plus the fragment the open terminated, which is no longer a tail — and the same 3 the open already reported", d)
	}
	if !d.Any() {
		t.Error("Any() is false on damage that has a number set")
	}
	if d.String() != "0 torn tail, 3 skipped" {
		t.Errorf("String() = %q", d.String())
	}

	// A TORN TAIL is still reachable through the port, and this is the shape
	// that produces one now that an open repairs what it finds: the file is
	// torn BEHIND a store that is already open — another writer killed
	// mid-Append, or this process about to be. Without this the tail counter
	// would be dead through the public API and only reachable in the parser.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, append(onDisk, []byte("{\"id\":\"rec-3\"")...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load after the second tear: %v", err)
	}
	if d := store.Damage(); d.TornTail != 1 {
		t.Errorf("damage = %s after the file was torn behind an open store, want 1 torn tail", d)
	}
}

// TestReopeningAfterATornTailDoesNotLandOnTheFragment is review round 2's
// blocking finding, and it is the one path a plugin actually takes after a
// crash: reopen the same file and append.
//
// A process killed inside Append leaves a line with no newline. O_APPEND puts
// the next write at the end of the FILE, not at the start of a line, so the
// recovering process's first event lands on the fragment and both are lost —
// and the recovering process's first event is the restart path's adopt, which
// durableWrite fsyncs precisely because nothing can re-derive it. One number
// was reported for two lost events, and nothing said that a freshly written
// one was among them.
func TestReopeningAfterATornTailDoesNotLandOnTheFragment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")

	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	first := lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Scope: "net-a", Family: lease.FamilyV4}
	second := lease.RecordEvent{ID: "rec-1", Seq: 2, Op: lease.OpBind}
	for _, ev := range []lease.RecordEvent{first, second} {
		if err := s.Append(ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The crash: killed inside the second Append, so its line is a fragment
	// with no newline. Cut it in the middle rather than at a boundary.
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.SplitAfter(whole, []byte("\n"))
	if len(lines) != 3 || len(lines[1]) < 4 {
		t.Fatalf("the fixture is not two lines: %q", whole)
	}
	torn := append(append([]byte(nil), lines[0]...), lines[1][:len(lines[1])/2]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The recovering process. Its FIRST event is the one nothing can
	// re-derive, which is why this is the event the defect destroyed.
	s, err = OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore after the crash: %v", err)
	}
	defer func() { _ = s.Close() }()
	if d := s.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage at open = %s, want 1 skipped: the open terminated a fragment and must say so, "+
			"so that its count and the next Load's are the same number for the same line", d)
	}
	adopt := lease.RecordEvent{ID: "rec-9", Seq: 1, Op: lease.OpAdopt, Scope: "net-a", Family: lease.FamilyV4}
	after := lease.RecordEvent{ID: "rec-2", Seq: 1, Op: lease.OpCreate, Scope: "net-a", Family: lease.FamilyV4}
	for _, ev := range []lease.RecordEvent{adopt, after} {
		if err := s.Append(ev); err != nil {
			t.Fatalf("Append after the crash: %v", err)
		}
	}

	evs, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids := make([]string, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID + "/" + ev.Op.String()
	}
	want := []string{"rec-1/create", "rec-9/adopt", "rec-2/create"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("Load returned %v, want %v: the recovering process's own first event was destroyed by the fragment it landed on", ids, want)
	}
	if d := s.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage after the Load = %s, want 1 skipped — the SAME line the open counted, counted the same way", d)
	}

	rb := lease.Rebuild(evs)
	if len(rb.Rejects) != 0 {
		t.Errorf("Rebuild reported %d reject(s) on a journal with one lost line and two good new ones: %v", len(rb.Rejects), rb.Rejects)
	}
	if len(rb.Records) != 3 {
		t.Fatalf("%d record(s) rebuilt, want 3", len(rb.Records))
	}
	if _, ok := rb.ByID("rec-9"); !ok {
		t.Error("the adopted record is not in the rebuild: the event the sync policy fsyncs was the one that was lost")
	}
}

// TestAFileThatEndsAtALineBoundaryIsNotRepaired is the other direction of the
// open-time repair, and without it the repair could be a newline appended to
// every file that is ever opened twice.
//
// A file whose last byte IS a newline has nothing half-written at the end. It
// must be left exactly as it is — no terminator, no damage counted — because a
// store that grew a blank line and a damage count on every restart would make
// the number meaningless and the file's history a record of its own opens.
func TestAFileThatEndsAtALineBoundaryIsNotRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")

	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Opened and closed three times over: neither the bytes nor the count move.
	for i := range 3 {
		s, err = OpenRecordStore(path)
		if err != nil {
			t.Fatalf("re-open %d: %v", i+1, err)
		}
		if d := s.Damage(); d.Any() {
			t.Errorf("re-open %d reported %s on a file that ends at a line boundary", i+1, d)
		}
		evs, err := s.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(evs) != 1 {
			t.Errorf("re-open %d: %d event(s), want 1", i+1, len(evs))
		}
		if d := s.Damage(); d.Any() {
			t.Errorf("re-open %d: Load reported %s", i+1, d)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("three opens changed the file:\nbefore %q\nafter  %q", before, after)
	}

	// An EMPTY file is the same case for the same reason: there is nothing
	// before the first event for it to run into.
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err = OpenRecordStore(empty)
	if err != nil {
		t.Fatalf("OpenRecordStore on an empty file: %v", err)
	}
	defer func() { _ = s.Close() }()
	if d := s.Damage(); d.Any() {
		t.Errorf("opening an empty file reported %s", d)
	}
	if b, err := os.ReadFile(empty); err != nil || len(b) != 0 {
		t.Errorf("opening an empty file wrote %q (err %v)", b, err)
	}
}

// TestTwoConsecutiveTornOpensCountTwoLostLines is the repair applied to its own
// output: a crash after a recovery is not a special case, and the counts must
// keep adding up line for line rather than resetting or double-counting.
func TestTwoConsecutiveTornOpensCountTwoLostLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")

	// tear cuts the file in the middle of its last line, which is what a
	// process killed inside Append leaves behind.
	tear := func() {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		lines := bytes.SplitAfter(b, []byte("\n"))
		last := lines[len(lines)-1]
		if len(last) != 0 {
			t.Fatalf("the file already ends mid-line: %q", last)
		}
		last = lines[len(lines)-2]
		if len(last) < 4 {
			t.Fatalf("the last line is too short to cut: %q", last)
		}
		cut := len(b) - len(last) + len(last)/2
		if err := os.WriteFile(path, b[:cut], 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	write := func(s *RecordStore, id string) {
		t.Helper()
		if err := s.Append(lease.RecordEvent{ID: id, Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// Round 1: two events, then a crash inside the second.
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	write(s, "rec-1")
	write(s, "rec-2")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tear()

	// Round 2: recover, write one event, then crash inside a second.
	s, err = OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, second: %v", err)
	}
	if d := s.Damage(); d.Skipped != 1 {
		t.Errorf("the first recovery reported %s, want 1 skipped", d)
	}
	write(s, "rec-3")
	write(s, "rec-4")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tear()

	// Round 3: recover again. A repairing open reports what the file holds and
	// not what it personally terminated, so this one already says 2 — the
	// fragment it just terminated and the one the previous open did — and the
	// Load below says 2 as well.
	s, err = OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, third: %v", err)
	}
	defer func() { _ = s.Close() }()
	if d := s.Damage(); d.Skipped != 2 || d.TornTail != 0 {
		t.Errorf("the second recovery reported %s, want 2 skipped: a repairing open reports what the next Load will, which is both broken lines", d)
	}
	write(s, "rec-5")

	evs, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids := make([]string, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID
	}
	want := []string{"rec-1", "rec-3", "rec-5"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("Load returned %v, want %v: one event was lost to each crash and no more", ids, want)
	}
	if d := s.Damage(); d.Skipped != 2 || d.TornTail != 0 {
		t.Fatalf("damage = %s after two crashes, want 2 skipped — one line per crash, neither counted twice", d)
	}
	if rb := lease.Rebuild(evs); len(rb.Rejects) != 0 || len(rb.Records) != 3 {
		t.Errorf("Rebuild: %d record(s), %d reject(s); want 3 and 0", len(rb.Records), len(rb.Rejects))
	}
}

// TestTheTerminatorIsSyncedBeforeTheFirstAppend pins the ORDER the open-time
// repair depends on, which is otherwise invisible: an fsync leaves no trace a
// same-process test can read, so it is observed the way every other fsync in
// this file is — through the indirection the call goes through.
//
// The terminator is not left to the first durable Append's fsync to carry.
// Two unsynced writes to one file are not ordered against each other across a
// power loss, so the event's bytes could reach the disk while the newline
// before them did not — which is the defect the terminator exists to prevent,
// reassembled out of its own remedy.
func TestTheTerminatorIsSyncedBeforeTheFirstAppend(t *testing.T) {
	// The recorder reads the file AT THE MOMENT OF THE SYNC. Recording only
	// that a sync happened cannot tell an fsync of the terminated file from an
	// fsync taken just before the terminator was written, and the second one
	// makes the newline exactly as unsynced as having no fsync at all.
	type syncEvent struct {
		name       string
		terminated bool
	}
	var synced []syncEvent
	real := syncRecordFile
	syncRecordFile = func(f *os.File) error {
		e := syncEvent{name: f.Name()}
		if fi, err := f.Stat(); err == nil && !fi.IsDir() {
			if b, err := os.ReadFile(f.Name()); err == nil {
				e.terminated = bytes.HasSuffix(b, []byte("\n"))
			}
		}
		synced = append(synced, e)
		return real(f)
	}
	t.Cleanup(func() { syncRecordFile = real })

	dir := t.TempDir()
	path := filepath.Join(dir, "records.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"rec-1\",\"op\":1,\"seq\":1}\n{\"id\":"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	synced = nil
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	if len(synced) != 1 || synced[0].name != path {
		t.Fatalf("opening a torn file synced %v, want exactly the journal %s — before any Append, not after", synced, path)
	}
	if !synced[0].terminated {
		t.Fatalf("the journal was fsynced while it still ended mid-line: the terminator went in AFTER its own sync, so it is no more durable than an unsynced write")
	}

	// The file on disk is already terminated at this point, with no Append yet:
	// the newline is out and durable before anything depends on it.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("the file still ends mid-line after the open: %q", b)
	}

	// And a file that needs no repair pays no fsync: the cost is on the path
	// that follows a crash, not on every process start.
	clean := filepath.Join(dir, "clean.jsonl")
	if err := os.WriteFile(clean, []byte("{\"id\":\"rec-1\",\"op\":1,\"seq\":1}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	synced = nil
	c, err := OpenRecordStore(clean)
	if err != nil {
		t.Fatalf("OpenRecordStore on a clean file: %v", err)
	}
	defer func() { _ = c.Close() }()
	if len(synced) != 0 {
		t.Errorf("opening a file that ends at a line boundary synced %v, want nothing", synced)
	}

	// A terminator that cannot be made durable fails the OPEN. Reporting
	// success would promise the ordering that was not obtained.
	syncRecordFile = func(f *os.File) error { return os.ErrPermission }
	if err := os.WriteFile(path, []byte("{\"id\":"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, err := OpenRecordStore(path); err == nil {
		_ = got.Close()
		t.Error("OpenRecordStore succeeded although the terminator could not be synced")
	}
}

// TestAnOpenStoreDoesNotAppendOntoAFragmentThatAppearedUnderIt is review round
// 3's blocking finding, scenario (b): the crash moved one step, to where the
// open-time repair by construction cannot see it.
//
// Round 3 repaired a fragment found at open. A fragment can also appear while
// the store is ALREADY open — another process on the same file died inside its
// Append (defeat row D-9) — and O_APPEND then puts this store's next event on
// the end of that half line, destroying both. The event this store writes next
// after a peer dies is the restart path's adopt, which durableWrite fsyncs
// precisely because nothing can re-derive it.
//
// The tear here is done to the file rather than by killing a process; the two
// tests below drive a real short write and a real kill.
func TestAnOpenStoreDoesNotAppendOntoAFragmentThatAppearedUnderIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")

	dying, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, the writer that dies: %v", err)
	}
	survivor, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, the writer that stays up: %v", err)
	}
	defer func() { _ = survivor.Close() }()

	for _, id := range []string{"rec-1", "rec-2"} {
		if err := dying.Append(lease.RecordEvent{ID: id, Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	if err := dying.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// It died inside its second Append: that line is now half written.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	last := lines[len(lines)-2]
	cut := len(b) - len(last) + len(last)/2
	if err := os.WriteFile(path, b[:cut], 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The survivor never reopened anything. Its next event is the adopt.
	adopt := lease.RecordEvent{ID: "rec-9", Seq: 1, Op: lease.OpAdopt, Scope: "net-a", Family: lease.FamilyV4}
	if err := survivor.Append(adopt); err != nil {
		t.Fatalf("the survivor's Append: %v", err)
	}

	// The repair happened inside that Append, so the count is readable BEFORE
	// any Load — and it is already what the Load below reports. Without this
	// the store can write the terminator and count nothing, and then say
	// "no damage" about a file it has just repaired.
	if d := survivor.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage after the repairing Append and before any Load = %s, want 1 skipped", d)
	}

	evs, err := survivor.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rb := lease.Rebuild(evs)
	if _, ok := rb.ByID("rec-9"); !ok {
		ids := make([]string, len(evs))
		for i, ev := range evs {
			ids[i] = ev.ID + "/" + ev.Op.String()
		}
		t.Fatalf("the fsynced adopt is gone; Load returned %v. The survivor wrote it onto the dead writer's half line and lost both", ids)
	}
	if _, ok := rb.ByID("rec-1"); !ok {
		t.Error("the line written before the crash is gone; a repair must not cost what was already there")
	}
	if len(rb.Rejects) != 0 {
		t.Errorf("Rebuild reported %d reject(s); a torn line is not an event the fold ever sees", len(rb.Rejects))
	}
	// One line was lost — the one the dead writer half wrote — and the count
	// says one, not one standing for two.
	if d := survivor.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage = %s, want exactly 1 skipped: the dead writer's half line and nothing of the survivor's", d)
	}
}

// TestATailThatIsAWholeEventIsNotCountedAsDamage is review round 3's second
// finding: one fact was derived twice and the two answers disagreed.
//
// parseRecordLines keeps a final line that has no newline but DOES parse — a
// whole event whose terminator alone was lost — and says why. The open counted
// that same line as a skipped line, so Damage said 1 and the Load immediately
// after it said 0 about the same file. The open now derives its number with the
// parse Load uses, which is the only construction under which the two cannot
// drift apart again.
func TestATailThatIsAWholeEventIsNotCountedAsDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	for _, id := range []string{"rec-1", "rec-2"} {
		if err := s.Append(lease.RecordEvent{ID: id, Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Drop ONLY the final newline. Both events are whole.
	if err := os.WriteFile(path, b[:len(b)-1], 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s2, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, second: %v", err)
	}
	defer func() { _ = s2.Close() }()

	atOpen := s2.Damage()
	if atOpen.Any() {
		t.Errorf("the open reported %s for a tail that is a whole event; parseRecordLines keeps such a tail on purpose", atOpen)
	}
	evs, err := s2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("Load returned %d event(s), want 2: the tail is a whole event", len(evs))
	}
	if got := s2.Damage(); got != atOpen {
		t.Errorf("the open said %s and the Load right after it says %s about the same file", atOpen, got)
	}
}

// TestDamageAfterARepairIsWhatTheNextLoadReports drives the contract Damage now
// states, in both of its halves and over every torn shape the reviewers found.
//
// Half one: where the store had to REPAIR, its number is what the next Load
// reports — asserted as an equality AND against a stated number, because two
// derivations that are wrong in the same direction satisfy an equality alone.
//
// Half two, the bound: Damage is not a survey. A file whose damage needed no
// repair — a broken line that still has its newline — reads 0 until a Load,
// because surveying a journal on every process start is what the one-byte
// check at open exists to avoid.
func TestDamageAfterARepairIsWhatTheNextLoadReports(t *testing.T) {
	good := []byte("{\"id\":\"rec-1\",\"op\":1,\"seq\":1}\n")
	for _, c := range []struct {
		what      string
		body      []byte
		repairs   bool
		wantOpen  lease.StoreDamage
		wantAfter lease.StoreDamage
	}{
		{"a fragment that does not parse", append(append([]byte(nil), good...), []byte("{\"id\":")...),
			true, lease.StoreDamage{Skipped: 1}, lease.StoreDamage{Skipped: 1}},
		{"a fragment of one byte, alone", []byte("{"),
			true, lease.StoreDamage{Skipped: 1}, lease.StoreDamage{Skipped: 1}},
		{"a tail that is a whole event", []byte("{\"id\":\"rec-1\",\"op\":1,\"seq\":1}"),
			true, lease.StoreDamage{}, lease.StoreDamage{}},
		{"a broken interior line and a fragment",
			append(append(append([]byte(nil), good...), []byte("not json\n")...), []byte("{\"id\":")...),
			true, lease.StoreDamage{Skipped: 2}, lease.StoreDamage{Skipped: 2}},
		{"a broken last line that kept its newline", append(append([]byte(nil), good...), []byte("not json\n")...),
			false, lease.StoreDamage{}, lease.StoreDamage{Skipped: 1}},
		{"a whole file", good,
			false, lease.StoreDamage{}, lease.StoreDamage{}},
	} {
		t.Run(c.what, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "records.jsonl")
			if err := os.WriteFile(path, c.body, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			s, err := OpenRecordStore(path)
			if err != nil {
				t.Fatalf("OpenRecordStore: %v", err)
			}
			defer func() { _ = s.Close() }()

			atOpen := s.Damage()
			if atOpen != c.wantOpen {
				t.Fatalf("Damage at open = %s, want %s", atOpen, c.wantOpen)
			}
			if _, err := s.Load(); err != nil {
				t.Fatalf("Load: %v", err)
			}
			after := s.Damage()
			if after != c.wantAfter {
				t.Fatalf("Damage after Load = %s, want %s", after, c.wantAfter)
			}
			if c.repairs && atOpen != after {
				t.Fatalf("the open repaired and reported %s; the Load right after it reports %s", atOpen, after)
			}
			if !c.repairs && atOpen.Any() {
				t.Fatalf("nothing was repaired, yet the open reported %s; Damage is not a survey", atOpen)
			}
		})
	}
}

// TestARepairedAppendIsStillOneWrite drives the property another writer on the
// same file depends on, in the case this round added.
//
// lease.Store says an Append must be atomic against a concurrent Append from
// another process: one line, one write. O_APPEND gives that only because the
// kernel takes the offset and the write together — so a repaired Append that
// wrote its terminator and then its event would open a window in which another
// process's whole line lands between them, and the reader would see a blank
// line followed by an event that is fine. Harmless there; not harmless as a
// precedent, because the same split applied to the event itself is the
// corruption this file exists against.
//
// The file that results is identical either way, which is why this counts the
// CALLS and not the bytes.
func TestARepairedAppendIsStillOneWrite(t *testing.T) {
	real := writeRecordLine
	var calls [][]byte
	writeRecordLine = func(f *os.File, b []byte) (int, error) {
		calls = append(calls, append([]byte(nil), b...))
		return real(f, b)
	}
	defer func() { writeRecordLine = real }()

	path := filepath.Join(t.TempDir(), "records.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"rec-1\",\"op\":1,\"seq\":1}\n{\"id\":"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	if len(calls) != 1 {
		t.Fatalf("the open made %d write(s); it terminates the fragment with one", len(calls))
	}

	calls = nil
	if err := s.Append(lease.RecordEvent{ID: "rec-9", Seq: 1, Op: lease.OpAdopt, Scope: "net-a"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("an ordinary Append made %d write(s), want 1", len(calls))
	}

	// Now tear the file behind the open store, so the next Append has to
	// repair, and count again.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, append(onDisk, []byte("{\"id\":\"rec-3\"")...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	calls = nil
	if err := s.Append(lease.RecordEvent{ID: "rec-4", Seq: 1, Op: lease.OpAdopt, Scope: "net-a"}); err != nil {
		t.Fatalf("Append after the tear: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("a repairing Append made %d write(s), want 1: the terminator rides with the event", len(calls))
	}
	if got := calls[0]; got[0] != '\n' {
		t.Errorf("the repairing write starts with %q, want the terminator first", got[0])
	}
}
