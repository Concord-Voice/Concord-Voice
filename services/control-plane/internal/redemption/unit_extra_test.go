package redemption

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestNoopAudit_RecordsNothing exercises the fallback sink (NewIssuer
// substitutes it when no sink is supplied). It must never error and must not
// require a tx.
func TestNoopAudit_RecordsNothing(t *testing.T) {
	err := noopAudit{}.RecordGeneration(context.Background(), nil, GenerationAudit{
		GrantKind: GrantPremiumSubscription, Count: 1,
	})
	assert.NoError(t, err)
}

// TestNewIssuer_NilAuditSubstitutesNoop pins that NewIssuer never holds a nil
// sink (it would nil-panic in Issue otherwise).
func TestNewIssuer_NilAuditSubstitutesNoop(t *testing.T) {
	iss := NewIssuer(nil, NewCatalog(), nil)
	require.NotNil(t, iss)
	require.NotNil(t, iss.audit, "nil audit sink must be replaced with noopAudit")
}

// TestCatalog_RegisterOverride covers the exact-match registration seam.
func TestCatalog_RegisterOverride(t *testing.T) {
	c := NewCatalog()
	called := false
	c.Register("custom:thing", func(_ context.Context, _ *sql.Tx, _ uuid.UUID, _ string, _ map[string]any) (GrantResult, error) {
		called = true
		return GrantResult{Description: "custom"}, nil
	})
	require.True(t, c.Supports("custom:thing"))

	effect, err := c.lookup("custom:thing")
	require.NoError(t, err)
	res, err := effect(context.Background(), nil, uuid.Nil, "custom:thing", nil)
	require.NoError(t, err)
	assert.Equal(t, "custom", res.Description)
	assert.True(t, called)
}

// TestDecodeParams covers the empty/null and integer-fidelity branches.
func TestDecodeParams(t *testing.T) {
	m, err := decodeParams(nil)
	require.NoError(t, err)
	assert.Empty(t, m)

	m, err = decodeParams([]byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, m)

	m, err = decodeParams([]byte(`{"months": 12}`))
	require.NoError(t, err)
	assert.Equal(t, 12, monthsFromParams(m), "UseNumber preserves integer fidelity")

	_, err = decodeParams([]byte(`{not json`))
	assert.Error(t, err)
}

// TestIsUniqueViolation_NonPQError returns false for a plain error.
func TestIsUniqueViolation_NonPQError(t *testing.T) {
	assert.False(t, isUniqueViolation(nil))
	assert.False(t, isUniqueViolation(context.Canceled))
}

// TestHandleRedeemError_Mapping covers the three error branches WITHOUT a DB:
// generic-invalid → 400 with the single message, already-redeemed → 409, and an
// unexpected (e.g. post-commit notify) error → 500. This pins the no-oracle 400
// message and the status-code contract.
func TestHandleRedeemError_Mapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}
	uid := uuid.New()

	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"generic invalid", ErrCodeNotValid, http.StatusBadRequest, genericInvalidMessage},
		{"already redeemed", ErrAlreadyRedeemed, http.StatusConflict, ""},
		{"unexpected error", context.DeadlineExceeded, http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			h.handleRedeemError(c, uid, tc.err)
			assert.Equal(t, tc.wantCode, w.Code)
			if tc.wantMsg != "" {
				assert.Contains(t, w.Body.String(), tc.wantMsg)
			}
		})
	}
}

// TestValidateSpec_CountBounds pins the allocation-size guard: a non-positive
// count and a count above MaxBatchSize are both rejected BEFORE any DB write or
// allocation. The over-limit case is the CWE-789 memory-exhaustion guard
// (CodeQL go/uncontrolled-allocation-size). No DB needed — validateSpec is pure.
func TestValidateSpec_CountBounds(t *testing.T) {
	iss := NewIssuer(nil, NewCatalog(), nil)

	t.Run("zero count rejected", func(t *testing.T) {
		err := iss.validateSpec(IssueSpec{GrantKind: GrantPremiumSubscription, Count: 0})
		assert.ErrorIs(t, err, errIssueCountInvalid)
	})
	t.Run("negative count rejected", func(t *testing.T) {
		err := iss.validateSpec(IssueSpec{GrantKind: GrantPremiumSubscription, Count: -5})
		assert.ErrorIs(t, err, errIssueCountInvalid)
	})
	t.Run("count over MaxBatchSize rejected", func(t *testing.T) {
		err := iss.validateSpec(IssueSpec{GrantKind: GrantPremiumSubscription, Count: MaxBatchSize + 1})
		assert.ErrorIs(t, err, errIssueCountTooLarge)
	})
	t.Run("count at MaxBatchSize accepted (single-use)", func(t *testing.T) {
		err := iss.validateSpec(IssueSpec{
			GrantKind: GrantPremiumSubscription, Count: MaxBatchSize, SingleUse: true, MaxRedeems: intPtrLocal(1),
		})
		assert.NoError(t, err)
	})
}

// TestGenerate_RejectsOverLimitCount pins the HTTP boundary guard: an over-limit
// count is rejected with a 400 BEFORE the issuer (and its allocation) is
// reached, so the DB is never touched (the handler holds a nil issuer here).
func TestGenerate_RejectsOverLimitCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// nil issuer + nil engine: the boundary check must return before any of them
	// is dereferenced. A panic here would mean the guard ran too late.
	h := &Handler{log: logger.New("test")}

	cases := []struct {
		name  string
		count int
	}{
		{"over MaxBatchSize", MaxBatchSize + 1},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"grant_kind": GrantPremiumSubscription,
				"count":      tc.count,
			})
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/admin/redemption/codes", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Generate(c)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "maximum batch size")
		})
	}
}

func intPtrLocal(n int) *int { return &n }

// TestSanitizeID strips control chars (CWE-117 log-forging defense).
func TestSanitizeID(t *testing.T) {
	assert.Equal(t, "abc123", sanitizeID("abc\n123"))
	assert.Equal(t, "abc123", sanitizeID("abc\r\n123"))
	assert.Equal(t, "clean", sanitizeID("clean"))
	assert.Equal(t, "x", sanitizeID("x\x00\x7f"))
}

// stubNotifier is an in-package Notifier double for the notifyAfterCommit unit
// test (no DB needed). It returns a configurable error.
type stubNotifier struct {
	called bool
	err    error
}

func (s *stubNotifier) OnTierChange(context.Context, uuid.UUID, string, string) error {
	s.called = true
	return s.err
}

// TestNotifyAfterCommit covers the extracted step-5 helper WITHOUT a DB. It pins
// the post-commit contract:
//   - no tier change          → no notifier call, no error, description returned.
//   - tier change, notify ok   → notifier called, no error, description returned.
//   - tier change, notify fail → notifier called, NON-FATAL wrapped error returned
//     ALONGSIDE the (durable-grant) description — exactly the inline behavior the
//     S3776 refactor preserved.
func TestNotifyAfterCommit(t *testing.T) {
	ctx := context.Background()
	uid := uuid.New()

	t.Run("no tier change → notifier not called", func(t *testing.T) {
		n := &stubNotifier{}
		e := &Engine{notifier: n}
		out, err := e.notifyAfterCommit(ctx, uid, GrantResult{Description: "themes unlocked"})
		require.NoError(t, err)
		assert.Equal(t, "themes unlocked", out.Description)
		assert.False(t, n.called, "no tier change must not fire OnTierChange")
	})

	t.Run("tier change, notify success", func(t *testing.T) {
		n := &stubNotifier{}
		e := &Engine{notifier: n}
		out, err := e.notifyAfterCommit(ctx, uid, GrantResult{
			Description: "premium", TierChanged: true, OldTier: "free", NewTier: "premium",
		})
		require.NoError(t, err)
		assert.Equal(t, "premium", out.Description)
		assert.True(t, n.called)
	})

	t.Run("tier change, notify failure → non-fatal wrapped error + description", func(t *testing.T) {
		n := &stubNotifier{err: errors.New("ws down")}
		e := &Engine{notifier: n}
		out, err := e.notifyAfterCommit(ctx, uid, GrantResult{
			Description: "premium", TierChanged: true, OldTier: "free", NewTier: "premium",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "post-commit notify")
		assert.Equal(t, "premium", out.Description, "the durable grant's description is still returned")
		assert.True(t, n.called)
	})

	t.Run("tier change but nil notifier → no-op", func(t *testing.T) {
		e := &Engine{notifier: nil}
		out, err := e.notifyAfterCommit(ctx, uid, GrantResult{
			Description: "premium", TierChanged: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "premium", out.Description)
	})
}

// TestHandleIssueError_Default500 covers the non-validation (infra) branch of
// handleIssueError — an unexpected error maps to a generic 500. The validation
// branches are already exercised by the handler integration tests; this pins the
// default arm without a DB.
func TestHandleIssueError_Default500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.handleIssueError(c, context.DeadlineExceeded)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "code generation failed")
}

// TestHandleIssueError_ValidationBranch pins a representative validation arm maps
// to a 400 with the specific (non-oracle, admin-side) message.
func TestHandleIssueError_ValidationBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{log: logger.New("test")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.handleIssueError(c, errIssueGrantUnknown)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "grant_kind not supported")
}

// TestRevokeBatch_EmptyBatchID rejects an empty batch id BEFORE any DB call (the
// guard is a pure precondition), so a nil db handle is safe here.
func TestRevokeBatch_EmptyBatchID(t *testing.T) {
	iss := NewIssuer(nil, NewCatalog(), nil)
	n, err := iss.RevokeBatch(context.Background(), "")
	require.Error(t, err)
	assert.Equal(t, int64(0), n)
	assert.Contains(t, err.Error(), "batch_id required")
}

// failingWriter always errors, exercising WriteCSV's write-error path.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestWriteCSV_HeaderWriteError surfaces the csv writer error (the header write
// fails first). Pins WriteCSV's error wrapping without touching a DB.
func TestWriteCSV_HeaderWriteError(t *testing.T) {
	err := WriteCSV(failingWriter{}, []IssuedCode{{Plaintext: "AAA"}}, "batch", "kind")
	require.Error(t, err)
}

// TestWriteCSV_Success writes a well-formed CSV to a buffer (header + rows).
func TestWriteCSV_Success(t *testing.T) {
	var buf bytes.Buffer
	err := WriteCSV(&buf, []IssuedCode{{Plaintext: "AAA"}, {Plaintext: "BBB"}}, "b1", "premium:subscription")
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "code,batch_id,grant_kind")
	assert.Contains(t, out, "AAA,b1,premium:subscription")
	assert.Contains(t, out, "BBB,b1,premium:subscription")
}

// TestWriteCSV_FormulaInjectionNeutralised pins the CWE-1236 guard (Gitar
// review): operator-supplied batch_id / grant_kind (and a code) that begin with
// a spreadsheet formula trigger are prefixed with a single quote so an importer
// treats them as literal text rather than executing them.
func TestWriteCSV_FormulaInjectionNeutralised(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"equals", "=cmd()", "'=cmd()"},
		{"plus", "+1+1", "'+1+1"},
		{"minus", "-2", "'-2"},
		{"at", "@SUM(A1)", "'@SUM(A1)"},
		{"tab", "\tx", "'\tx"},
		{"cr", "\rx", "'\rx"},
		{"benign", "premium:subscription", "premium:subscription"},
		{"empty", "", ""},
	} {
		assert.Equal(t, tc.want, csvSafe(tc.in), tc.name)
	}

	// End-to-end: a malicious batch_id flows through WriteCSV neutralised — the
	// leading single quote is present so a spreadsheet treats the cell as text.
	var buf bytes.Buffer
	require.NoError(t, WriteCSV(&buf, []IssuedCode{{Plaintext: "AAA"}}, "=HYPERLINK(0)", "premium:subscription"))
	assert.Contains(t, buf.String(), "AAA,'=HYPERLINK(0),premium:subscription")
}
