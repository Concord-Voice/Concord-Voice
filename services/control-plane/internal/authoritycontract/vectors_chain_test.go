package authoritycontract

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	codecChecks["chain"] = checkChainVector
	codecChecks["bundle-verify"] = checkBundleVerifyVector
}

// trustOf builds a vector's Trust: NewTrust over `anchors` (a T1 failure is
// malformed), then AddSigned over every `trust` item in order. Like AddTrust,
// it admits every item even after a halt: T4 is a property of the retained
// set, recomputed after each admission, so a later item can end a halt and
// stopping at the first one would make the result depend on arrival order.
func trustOf(t *testing.T, v vector) (*Trust, Reason) {
	t.Helper()
	a, err := DecodeAnchors(mustB64(t, v.Anchors))
	require.NoError(t, err)
	tr, err := NewTrust(a)
	if err != nil {
		return nil, ReasonMalformed
	}
	for _, it := range v.Trust {
		err := tr.AddSigned(mustB64(t, it))
		var h *HaltError
		if err != nil && !errors.As(err, &h) && !errors.Is(err, ErrIgnored) && !errors.Is(err, ErrMalformed) {
			require.NoError(t, err)
		}
	}
	return tr, ReasonOK
}

// nowOf reads a vector's now_ms: a u, so at most 2^53 − 1 (EN1).
func nowOf(t *testing.T, ms uint64) time.Time {
	t.Helper()
	if ms > MaxSafeInt {
		t.Fatalf("now_ms %d is above 2^53 - 1", ms)
		return time.Time{}
	}
	return time.UnixMilli(int64(ms))
}

func prevHeadOf(t *testing.T, v vector) ChainHead {
	t.Helper()
	h, err := ParseChainHead(mustB64(t, v.PrevHead))
	require.NoError(t, err)
	return h
}

// chainInputOf is what a chain vector serves to VerifyChain.
func chainInputOf(t *testing.T, v vector) ChainInput {
	t.Helper()
	return ChainInput{Bundles: listB64(t, v.Bundles), Status: mustB64(t, v.BytesB64)}
}

func checkChainVector(t *testing.T, v vector) {
	tr, r := trustOf(t, v)
	if r != ReasonOK {
		require.Equal(t, v.Expect.Reason, string(r))
		return
	}
	use, ok := ParseUse(v.Use)
	require.True(t, ok)
	head, _, _, got := VerifyChain(tr, hwOf(v.HW), prevHeadOf(t, v), chainInputOf(t, v), memOf(t, v.Mem), nowOf(t, v.NowMs), use)
	require.Equal(t, v.Expect.Reason, string(got))
	if v.Expect.HeadB64 != "" {
		require.Equal(t, v.Expect.HeadB64, base64.StdEncoding.EncodeToString(head.Bytes()), "the persisted head (D-269)")
	}
}

func checkBundleVerifyVector(t *testing.T, v vector) {
	tr, r := trustOf(t, v)
	require.Equal(t, ReasonOK, r)
	var fix *Fix
	if v.Fix != "" {
		fix = &Fix{Status: mustB64(t, v.Fix)}
	}
	head, got := VerifyBundle(tr, hwOf(v.HW), prevHeadOf(t, v), mustB64(t, v.BytesB64), fix)
	require.Equal(t, v.Expect.Reason, string(got))
	if v.Expect.HeadB64 != "" {
		require.Equal(t, v.Expect.HeadB64, base64.StdEncoding.EncodeToString(head.Bytes()))
	}
}
