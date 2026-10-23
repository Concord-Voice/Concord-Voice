package authoritycontract

// AuthenticateBundle (#3587 PR-B3, decision B3-D15 R1) is VerifyBundle's
// prev-independent part: EN5 over every part, every part naming the Trust's
// realm and one account, the result binding its intent (request_id,
// op, intent and cp-authz digests), and T7's RSK signature, window and scope
// under a delegation the Trust retains at the result's (gen, serial). It
// reads no head, so a bundle any number of seqs ahead authenticates, a
// pinned account's first result is not refused for want of AccountMemory,
// and an entry made superseded by a renewal is reported, not refused.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// authForge builds a forge chain over a realm holding delegation serial 1,
// and a bundle for a result at seq 3 that links to nothing: AuthenticateBundle
// must not read prev_entry_digest or the seq.
func authForge(t *testing.T) (*forgeChain, []byte) {
	t.Helper()
	return authForgeOn(t, newForgeChain(t))
}

func authForgeOn(t *testing.T, c *forgeChain) (*forgeChain, []byte) {
	t.Helper()
	root := testKey(t)
	ekb, ekbD, ekbSig := c.ekBinding(root, 1, forgedEK(t))
	issued := testT0 + 60_000
	in := Intent{RequestID: newID(), Op: OpEnrollNew, ExpiresAtMs: issued + 600_000,
		CandidateRoot: pointOf(t, root), EKBindingDigest: ekbD, DeviceIntentDigest: digestOf("dev-1")}
	a := CPAuthz{Actor: ActorUser, AssertedAtMs: issued, EvidenceDigest: digestOf("ev-1")}
	res := Result{Seq: 3, PrevEntryDigest: digestOf("an entry no head holds"), State: StateActive,
		ActiveRoot: pointOf(t, root), EKBindingDigest: ekbD, Assurance: AssuranceInitial, IssuedAtMs: issued}
	b, _ := c.finish(in, a, res, forgeSigs{candidate: root, ekb: ekb, ekbSig: ekbSig})
	return c, b
}

func TestAuthenticateBundleAcceptsABundleWithNoHead(t *testing.T) {
	c, bundle := authForge(t)
	res, sup, r := AuthenticateBundle(c.tr, HighWater{}, bundle)
	require.Equal(t, ReasonOK, r)
	require.False(t, sup, "signed under the only delegation: not superseded")
	require.Equal(t, uint64(3), res.Seq, "the verified result is returned, so the caller reads seq and account from it")
	require.Equal(t, c.account, res.AccountID)
	require.Equal(t, c.realm, res.RealmID)
	d, err := DecodeBundle(bundle)
	require.NoError(t, err)
	want, err := DecodeResult(d.Result)
	require.NoError(t, err)
	require.Equal(t, want, res)
}

func TestAuthenticateBundleRefusals(t *testing.T) {
	t.Run("bad_signature: the result signature does not verify", func(t *testing.T) {
		c, bundle := authForge(t)
		broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, broken)
		require.Equal(t, ReasonBadSignature, r)
	})

	t.Run("bad_signature: a result signed by a key that is not an RSK", func(t *testing.T) {
		c, bundle := authForge(t)
		other := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = c.sign(testKey(t), b.Result) })
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, other)
		require.Equal(t, ReasonBadSignature, r)
	})

	t.Run("malformed: bytes that are not a bundle", func(t *testing.T) {
		c, _ := authForge(t)
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, []byte("not a bundle"))
		require.Equal(t, ReasonMalformed, r)
	})

	t.Run("realm_mismatch: every part names another realm than the Trust's", func(t *testing.T) {
		c := newForgeChain(t)
		c.realm = newID() // finish signs every part with it; the RSK still signs
		_, bundle := authForgeOn(t, c)
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonRealmMismatch, r)
	})

	t.Run("realm_mismatch: the intent names another account than the result", func(t *testing.T) {
		c, bundle := authForge(t)
		swapped := rebundle(t, bundle, func(b *Bundle) {
			in, err := DecodeIntent(b.Intent)
			require.NoError(t, err)
			in.AccountID = newID()
			b.Intent = in.Encode()
		})
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, swapped)
		require.Equal(t, ReasonRealmMismatch, r)
	})

	t.Run("realm_mismatch: the cp-authz names another realm than the result", func(t *testing.T) {
		c, bundle := authForge(t)
		swapped := rebundle(t, bundle, func(b *Bundle) {
			a, err := DecodeCPAuthz(b.CPAuthz)
			require.NoError(t, err)
			a.RealmID = newID()
			b.CPAuthz = a.Encode()
		})
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, swapped)
		require.Equal(t, ReasonRealmMismatch, r)
	})

	t.Run("transition_invalid: the result does not bind its intent", func(t *testing.T) {
		c, bundle := authForge(t)
		// A validly encoded intent for the same account, beside the RSK-signed
		// result that binds a different intent digest: the signature still verifies.
		swapped := rebundle(t, bundle, func(b *Bundle) {
			in, err := DecodeIntent(b.Intent)
			require.NoError(t, err)
			in.RequestID = newID()
			b.Intent = in.Encode()
		})
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, swapped)
		require.Equal(t, ReasonTransitionInvalid, r)
	})

	t.Run("transition_invalid: the cp-authz does not bind the intent", func(t *testing.T) {
		c, bundle := authForge(t)
		swapped := rebundle(t, bundle, func(b *Bundle) {
			a, err := DecodeCPAuthz(b.CPAuthz)
			require.NoError(t, err)
			a.EvidenceDigest = digestOf("other evidence")
			b.CPAuthz = a.Encode()
		})
		_, _, r := AuthenticateBundle(c.tr, HighWater{}, swapped)
		require.Equal(t, ReasonTransitionInvalid, r, "the result's cp_authz_digest no longer matches")
	})

	t.Run("delegation_unknown: the Trust retains no delegation at the result's serial", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(2, nil) // the result names (0, 1)
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		_, bundle := authForgeOn(t, c)
		_, _, got := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonDelegationUnknown, got)
	})

	t.Run("delegation_window: the result was issued after the delegation's window", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, func(d *Delegation) { d.NotAfterMs = testT0 + 1_000 })
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		_, bundle := authForgeOn(t, c)
		_, _, got := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonDelegationWindow, got)
	})

	t.Run("out_of_scope: the delegation's scope omits the result's op", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, func(d *Delegation) {
			d.Scope = slices.DeleteFunc(slices.Clone(scopeValues), func(s string) bool { return s == OpEnrollNew })
		})
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		_, bundle := authForgeOn(t, c)
		_, _, got := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonOutOfScope, got)
	})

	t.Run("trust_halt: a halted Trust admits nothing, and the halt outranks the bundle's own Reason", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, nil)
		r.succeed(nil) // a succession to gen 1 with no gen-1 delegation (T4)
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		require.NotNil(t, c.tr.Halted(), "precondition: T4 halts the realm")
		// The bundle is signed by gen 0's delegation, so it would otherwise verify.
		_, bundle := authForgeOn(t, c)
		_, _, got := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonTrustHalt, got)
		broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })
		_, _, got = AuthenticateBundle(c.tr, HighWater{}, broken)
		require.Equal(t, ReasonTrustHalt, got)
	})

	t.Run("trust_halt: a nil Trust", func(t *testing.T) {
		_, bundle := authForge(t)
		_, _, got := AuthenticateBundle(nil, HighWater{}, bundle)
		require.Equal(t, ReasonTrustHalt, got)
	})

	t.Run("a refusal returns the zero Result", func(t *testing.T) {
		c, bundle := authForge(t)
		broken := rebundle(t, bundle, func(b *Bundle) { b.ResultSig = flipped(b.ResultSig) })
		res, sup, r := AuthenticateBundle(c.tr, HighWater{}, broken)
		require.NotEqual(t, ReasonOK, r)
		require.False(t, sup)
		require.Equal(t, Result{}, res, "a caller that ignores the Reason must not read an unverified account or seq")
	})
}

// The same refusals through the committed corpus: each vector below reaches
// its Reason in VerifyBundle at T7 or in the identity check, both of which
// AuthenticateBundle shares, so the Reason carries over unchanged.
func TestAuthenticateBundleRefusesCorpusVectors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason Reason
	}{
		{"neg/transition/result-signature", ReasonBadSignature},
		{"trust/window/t7-at-not-after", ReasonDelegationWindow},
		{"neg/transition/op-out-of-scope", ReasonOutOfScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := vectorOf(t, tc.name, "bundle-verify")
			require.Equal(t, string(tc.reason), v.Expect.Reason, "the vector's own verdict")
			tr, r := trustOf(t, v)
			require.Equal(t, ReasonOK, r)
			_, sup, got := AuthenticateBundle(tr, hwOf(v.HW), mustB64(t, v.BytesB64))
			require.Equal(t, tc.reason, got)
			require.False(t, sup)
		})
	}
}

// T7: an entry signed under a serial a later renewal (min_valid_serial) has
// passed is superseded. VerifyBundle refuses it as superseded_uncovered;
// AuthenticateBundle reports it as (result, true, OK) and leaves admitting it
// to the caller, because hold-req refuses it and import admits it.
func TestAuthenticateBundleReportsASupersededEntry(t *testing.T) {
	t.Run("a renewal installed after the result", func(t *testing.T) {
		r := newTestRealm(t)
		r.addDel(1, nil)
		r.addDel(2, nil) // MinValidSerial = 2: every result under serial 1 is superseded
		c := &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
		require.Equal(t, uint64(2), c.tr.HighWater().MinValid, "precondition: the renewal raised min_valid")
		_, bundle := authForgeOn(t, c)
		res, sup, got := AuthenticateBundle(c.tr, HighWater{}, bundle)
		require.Equal(t, ReasonOK, got)
		require.True(t, sup)
		require.Equal(t, uint64(3), res.Seq)
		require.Equal(t, c.account, res.AccountID)
	})

	t.Run("a persisted high-water mark past the result's serial", func(t *testing.T) {
		c, bundle := authForge(t)
		_, sup, got := AuthenticateBundle(c.tr, HighWater{Gen: 0, Serial: 2, MinValid: 2}, bundle)
		require.Equal(t, ReasonOK, got)
		require.True(t, sup, "the caller's HighWater supersedes as the Trust's own does")
	})

	t.Run("the committed succession vector VerifyBundle refuses", func(t *testing.T) {
		cv := vectorOf(t, "trust/t7/succession-serial-reset-covered", "chain")
		ci := chainInputsOf(t, cv)
		bundle := mustB64(t, cv.Bundles[0])
		_, vr := VerifyBundle(ci.tr, ci.hw, ci.prev, bundle, nil)
		require.Equal(t, ReasonSupersededUncovered, vr, "control")
		_, sup, got := AuthenticateBundle(ci.tr, ci.hw, bundle)
		require.Equal(t, ReasonOK, got)
		require.True(t, sup)
	})
}

// AuthenticateBundle is VerifyBundle with the head-dependent checks removed,
// so it must accept everything VerifyBundle accepts, and report superseded
// exactly where VerifyBundle says superseded_uncovered. A divergence is a
// vector the two functions disagree about, which is AC9's drift.
func TestAuthenticateBundleAgreesWithVerifyBundleOnTheCorpus(t *testing.T) {
	accepted, superseded := 0, 0
	for _, v := range loadVectors(t).Vectors {
		var bundles [][]byte
		switch v.Type {
		case "bundle-verify":
			bundles = [][]byte{mustB64(t, v.BytesB64)}
		case "chain":
			if len(v.Bundles) == 0 {
				continue
			}
			bundles = [][]byte{mustB64(t, v.Bundles[0])} // the only bundle with a head to verify against
		default:
			continue
		}
		tr, r := trustOf(t, v)
		if r != ReasonOK {
			continue
		}
		_, vr := VerifyBundle(tr, hwOf(v.HW), prevHeadOf(t, v), bundles[0], nil)
		if vr != ReasonOK && vr != ReasonSupersededUncovered {
			continue
		}
		res, sup, got := AuthenticateBundle(tr, hwOf(v.HW), bundles[0])
		require.Equal(t, ReasonOK, got, "%s: VerifyBundle says %s", v.Name, vr)
		require.Equal(t, vr == ReasonSupersededUncovered, sup, "%s: the superseded bit", v.Name)
		d, err := DecodeBundle(bundles[0])
		require.NoError(t, err)
		want, err := DecodeResult(d.Result)
		require.NoError(t, err)
		require.Equal(t, want, res, "%s: the verified result", v.Name)
		accepted++
		if sup {
			superseded++
		}
	}
	require.Greater(t, accepted, 50, "the corpus has dozens of accepted bundles")
	require.Positive(t, superseded, "the corpus has superseded entries, so the bit is exercised both ways")
}

// ---- Go only, for the issuing authority ----

// authorityOnly are the exports §2.12 lists as Go only for the authority
// (B3-D15). A client that called AuthenticateBundle in place of VerifyBundle
// would move no head through a link and would admit a superseded entry with no
// covering status, so no package outside internal/authority/ may name one.
// ReplayAuthorityBundle is the same: a client that called it would take a
// pin it was never given and admit a superseded entry with no covering status.
// AuthenticateDeviceBundle (B3-D17) is the same for a private entry: a client
// that called it in place of VerifyDeviceLedger would admit an entry without
// the device-status, the link or DR3.
var authorityOnly = []string{"AuthenticateBundle", "ReplayAuthorityBundle", "AuthenticateDeviceBundle"}

// authorityOnlyRefs counts the identifiers in src that name an authorityOnly
// export, as a qualified selector, a dot-imported call or a method value; a
// comment names nothing. A source that does not parse is an error.
func authorityOnlyRefs(src []byte) (int, error) {
	if !slices.ContainsFunc(authorityOnly, func(n string) bool { return bytes.Contains(src, []byte(n)) }) {
		return 0, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return 0, err
	}
	n := 0
	ast.Inspect(f, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && slices.Contains(authorityOnly, id.Name) {
			n++
		}
		return true
	})
	return n, nil
}

func TestAuthorityOnlyRefsFindsPlantedReferences(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"package x\nimport ac \"m/authoritycontract\"\nvar _, _, _ = ac.AuthenticateBundle(nil, ac.HighWater{}, nil)\n", 1},
		{"package x\nimport . \"m/authoritycontract\"\nvar f = AuthenticateBundle\n", 1},
		{"package x\n// AuthenticateBundle is named in a comment only.\nvar s = \"AuthenticateBundle\"\n", 0},
		{"package x\nimport ac \"m/authoritycontract\"\nvar _, _, _ = ac.AuthenticateDeviceBundle(nil, ac.HighWater{}, nil)\n", 1},
	} {
		got, err := authorityOnlyRefs([]byte(tc.src))
		require.NoError(t, err)
		require.Equal(t, tc.want, got, tc.src)
	}
}

func TestAuthorityOnlyExportsStayInTheAuthority(t *testing.T) {
	root := os.DirFS(filepath.Join("..", "..")) // services/control-plane, the module
	_, err := fs.Stat(root, "go.mod")
	require.NoError(t, err, "the walk must start at the module root")
	var outside []string
	inAuthority := 0
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != "." && (n == "node_modules" || n == "vendor" || strings.HasPrefix(n, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		n, err := authorityOnlyRefs(src)
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(p, "internal/authority/"):
			inAuthority += n
		case strings.HasPrefix(p, "internal/authoritycontract/"):
		case n > 0:
			outside = append(outside, p)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, outside, "a Go-only authority export is named outside internal/authority/")
	require.Positive(t, inAuthority, "the walk never saw the store's own call, so it proves nothing")
}
