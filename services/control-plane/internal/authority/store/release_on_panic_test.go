package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// CWE-772: serve's run() recovers a route's panic, answers 503
// store and keeps serving (B2-D7), and the tick keeps feeding the watchdog,
// so nothing restarts the process. A slot or a pooled connection a request
// path takes must therefore come back on a panic too, or each recovered
// panic holds one until restart.

// A decoder that panics under a general mutating slot gives the slot back:
// after MutatingSlots-1 recovered panics, an ordinary submit is still
// admitted. Before the fix those panics held every general slot, and the
// ordinary submit got 503 busy until restart.
func TestARecoveredDecodePanicReleasesItsSlot(t *testing.T) {
	s := &Store{adm: NewAdmission()}
	boom := func([]byte) (*request, Outcome, bool) { panic("decoder bug") }
	for range MutatingSlots - 1 {
		func() {
			defer func() { _ = recover() }() // as serve's run() recovers a non-abort panic
			s.admitAndDecode([]byte("{}"), boom)
		}()
	}
	ordinary := func([]byte) (*request, Outcome, bool) { return &request{Class: classOther}, Outcome{}, true }
	_, rel, out, ok := s.admitAndDecode([]byte("{}"), ordinary)
	if !ok {
		t.Fatalf("after %d recovered decode panics an ordinary submit got %d %q: the panics held the slots", MutatingSlots-1, out.HTTP, out.Code)
	}
	rel()
}

// A refusal still gives its general slot back exactly once, and an admitted
// request keeps its slot until its caller releases it: a leak shows as a
// missing slot below, a slot released on the success path as one too many.
func TestADecodeRefusalReleasesItsSlotOnce(t *testing.T) {
	s := &Store{adm: NewAdmission()}
	refuse := func([]byte) (*request, Outcome, bool) { return nil, Outcome{HTTP: 400}, false }
	for range 2 * MutatingSlots {
		if _, _, _, ok := s.admitAndDecode(nil, refuse); ok {
			t.Fatal("a refusal was admitted")
		}
	}
	admit := func([]byte) (*request, Outcome, bool) { return &request{Class: classOther}, Outcome{}, true }
	rels := make([]func(), 0, MutatingSlots-1)
	for range MutatingSlots - 1 {
		_, rel, _, ok := s.admitAndDecode(nil, admit)
		if !ok {
			t.Fatal("a general slot leaked on a refusal")
		}
		rels = append(rels, rel)
	}
	if _, _, _, ok := s.admitAndDecode(nil, admit); ok {
		t.Fatal("an ordinary submit was admitted past MutatingSlots-1 held general slots")
	}
	for _, r := range rels {
		r()
	}
}

// nopConn is a database connection that does nothing; database/sql pools it.
type nopConn struct{}

func (nopConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no statements") }
func (nopConn) Close() error                        { return nil }
func (nopConn) Begin() (driver.Tx, error)           { return nil, errors.New("no transactions") }

type nopConnector struct{}

func (nopConnector) Connect(context.Context) (driver.Conn, error) { return nopConn{}, nil }
func (nopConnector) Driver() driver.Driver                        { return nil }

// A read session whose leader check panics gives back both its read slot and
// its pooled connection; a session that passes hands both to its caller.
func TestARecoveredPanicInAReadSessionReleasesItsSlotAndConnection(t *testing.T) {
	db := sql.OpenDB(nopConnector{})
	t.Cleanup(func() { _ = db.Close() })
	panicking := true
	s := &Store{db: db, adm: NewAdmission(), hooks: Hooks{
		InRecovery: func(context.Context, *sql.Conn) (bool, error) {
			if panicking {
				panic("hook bug")
			}
			return false, nil
		},
		Statfs: func() (uint64, error) { return DiskHardFloorBytes * 2, nil },
	}}
	for range ReadSlots + 1 {
		func() {
			defer func() { _ = recover() }()
			s.readSession(context.Background())
		}()
	}
	if n := db.Stats().InUse; n != 0 {
		t.Fatalf("%d pooled connections held after recovered panics", n)
	}
	rels := make([]func(), 0, ReadSlots)
	for range ReadSlots {
		rel, ok := s.adm.acquireRead()
		if !ok {
			t.Fatal("a read slot was held after a recovered panic")
		}
		rels = append(rels, rel)
	}
	for _, r := range rels {
		r()
	}
	panicking = false
	conn, rel, _, ok := s.readSession(context.Background())
	if !ok || db.Stats().InUse != 1 {
		t.Fatalf("control: a passing session: ok %v, in use %d", ok, db.Stats().InUse)
	}
	_ = conn.Close()
	rel()
}

// seedVectors adds every authoritycontract test vector's bytes to f.
func seedVectors(f *testing.F) {
	f.Helper()
	raw, err := os.ReadFile("../../authoritycontract/testdata/vectors.json")
	if err != nil {
		f.Fatal(err)
	}
	var vs struct {
		Vectors []struct {
			BytesB64 string `json:"bytes_b64"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		f.Fatal(err)
	}
	if len(vs.Vectors) == 0 {
		f.Fatal("no vectors to seed")
	}
	for _, v := range vs.Vectors {
		if b, err := base64.StdEncoding.DecodeString(v.BytesB64); err == nil {
			f.Add(b)
		}
	}
}

// The submit decoder runs under a general mutating slot; a body that panics
// it would make the slot leak above reachable from a request. The seed
// corpus runs as an ordinary test.
func FuzzDecodeSubmit(f *testing.F) {
	seedVectors(f)
	f.Fuzz(func(_ *testing.T, b []byte) { _, _, _ = decodeSubmit(b) })
}

// The device-submit decoder, likewise.
func FuzzDecodeDeviceSubmit(f *testing.F) {
	seedVectors(f)
	f.Fuzz(func(_ *testing.T, b []byte) { _, _, _ = decodeDeviceSubmit(b) })
}
