package authoritytest

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"math/big"
	"sync"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/google/uuid"
)

// FakeEKSPKI is a fresh RSA-4096 SPKI with a random odd modulus (top bit
// set) and e = 65537. It validates (A2 Task 13's TestValidateEKSPKIAcceptsATemplateModulus)
// and is never reused, so every binding is fresh (D-243). B1 never decrypts to it.
func FakeEKSPKI(t testing.TB) []byte {
	t.Helper()
	nb := make([]byte, 512)
	if _, err := rand.Read(nb); err != nil {
		t.Fatal(err)
	}
	nb[0] |= 0x80
	nb[511] |= 0x01
	der, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: 65537})
	if err != nil || len(der) != 550 {
		t.Fatalf("authoritytest: EK SPKI is %d bytes, %v; want 550", len(der), err)
	}
	return der
}

// LegacyKey is an account's pre-enrollment RSA key: the private key, its SPKI and the pin digest.
type LegacyKey struct {
	Priv         *rsa.PrivateKey
	SPKI, Digest []byte
}

var (
	legacyMu   sync.Mutex
	legacyPool []LegacyKey
)

// NewLegacyKey returns a real RSA-4096 key (PoP needs a real PSS signature).
// RSA-4096 generation is slow, so the process keeps a pool of four and hands
// them out round-robin; tests that need distinct pins in one test take
// different calls.
func NewLegacyKey(t testing.TB) LegacyKey {
	t.Helper()
	legacyMu.Lock()
	defer legacyMu.Unlock()
	if len(legacyPool) < 4 {
		k, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		lk := LegacyKey{Priv: k, SPKI: der, Digest: ac.KeyDigest(der)}
		legacyPool = append(legacyPool, lk)
		return lk
	}
	lk := legacyPool[0]
	legacyPool = append(legacyPool[1:], lk)
	return lk
}

// Device is one device's ID and its signing (DSK) and key-exchange (DKX) keys.
type Device struct {
	ID       string
	DSK, DKX *ecdsa.PrivateKey
}

// NewDevice makes a device whose ID is DeviceID(dsk) (DR1).
func NewDevice(t testing.TB) *Device {
	t.Helper()
	dsk, dp := genKey(t)
	dkx, _ := genKey(t)
	var p [97]byte
	copy(p[:], dp)
	return &Device{ID: ac.DeviceID(p), DSK: dsk, DKX: dkx}
}

// HeadView is the fixture's copy of an account head; builders fill expected-head fields from it.
type HeadView struct {
	Seq, RG, EKGen, Dseq    uint64
	Entry, DeviceHead, Root []byte
	State                   string
}

// Account is one test account in a Realm: its keys, devices and view of the head.
type Account struct {
	R        *Realm
	ID       string
	Root     *ecdsa.PrivateKey
	Pending  *ecdsa.PrivateKey
	Legacy   *LegacyKey
	Devices  map[string]*Device
	Actor    *Device
	Recovery *ecdsa.PrivateKey
	Head     HeadView
}

// NewAccount adds an account with a fresh ID and no keys to the realm.
func (r *Realm) NewAccount(_ testing.TB) *Account {
	return &Account{R: r, ID: uuid.NewString(), Devices: map[string]*Device{}}
}

// Sync reads the account's head columns (any role with SELECT).
func (a *Account) Sync(t testing.TB, db *sql.DB) {
	t.Helper()
	var h HeadView
	err := db.QueryRow(`SELECT seq, recovery_gen, ek_generation, dseq, head_entry_digest, device_head_digest,
		active_root, state FROM authority.account_heads WHERE account_id = $1`, a.ID).
		Scan(&h.Seq, &h.RG, &h.EKGen, &h.Dseq, &h.Entry, &h.DeviceHead, &h.Root, &h.State)
	if err == sql.ErrNoRows {
		a.Head = HeadView{State: ac.StateAbsent}
		return
	}
	if err != nil {
		t.Fatalf("authoritytest: sync head: %v", err)
	}
	a.Head = h
}

// Activated records that the last candidate became the active root.
func (a *Account) Activated() {
	if a.Pending != nil {
		a.Root, a.Pending = a.Pending, nil
	}
}
