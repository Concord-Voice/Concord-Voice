package expiration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestValidate_AllSupportedShapes(t *testing.T) {
	oldRevision := int64(7)
	for _, window := range []int{3600, 86400, 604800, 2592000} {
		for _, retroactive := range []string{"apply", "new_only"} {
			t.Run("set "+retroactive, func(t *testing.T) {
				request := Request{Mode: "set", WindowSeconds: &window, Retroactive: retroactive}
				require.NoError(t, request.Validate())
			})
		}
	}
	for _, tc := range []struct {
		name    string
		request Request
	}{
		{"clear pending", Request{Mode: "clear", Retroactive: "clear_pending"}},
		{"clear leave pending", Request{Mode: "clear", Retroactive: "leave_pending"}},
		{"resume", Request{Mode: "resume", Revision: &oldRevision}},
	} {
		t.Run(tc.name, func(t *testing.T) { require.NoError(t, tc.request.Validate()) })
	}
}

func TestRequestValidate_RejectsInvalidCombinationsAndWindows(t *testing.T) {
	window := 86400
	badRevision := int64(-1)
	for _, tc := range []struct {
		name    string
		request Request
		want    error
	}{
		{"unknown mode", Request{Mode: "delete"}, ErrInvalidRequest},
		{"set missing window", Request{Mode: "set", Retroactive: "apply"}, ErrInvalidRequest},
		{"set unknown retroactive", Request{Mode: "set", WindowSeconds: &window, Retroactive: "clear_pending"}, ErrInvalidRequest},
		{"set with revision", Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply", Revision: &badRevision}, ErrInvalidRequest},
		{"clear with window", Request{Mode: "clear", WindowSeconds: &window, Retroactive: "clear_pending"}, ErrInvalidRequest},
		{"clear missing retroactive", Request{Mode: "clear"}, ErrInvalidRequest},
		{"resume missing revision", Request{Mode: "resume"}, ErrInvalidRequest},
		{"resume negative revision", Request{Mode: "resume", Revision: &badRevision}, ErrInvalidRequest},
		{"resume with retroactive", Request{Mode: "resume", Revision: ptr(int64(1)), Retroactive: "apply"}, ErrInvalidRequest},
		{"unsupported window", Request{Mode: "set", WindowSeconds: ptr(3601), Retroactive: "apply"}, ErrInvalidWindow},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.ErrorIs(t, tc.request.Validate(), tc.want) })
	}
}

func TestRequestJSONContract_RejectsUnknownFieldsAndMalformedTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"set", `{"mode":"set","window_seconds":86400,"retroactive":"apply"}`},
		{"clear", `{"mode":"clear","retroactive":"leave_pending"}`},
		{"resume", `{"mode":"resume","revision":7}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request Request
			require.NoError(t, json.Unmarshal([]byte(tc.body), &request))
			require.NoError(t, request.Validate())

			var withUnknown Request
			assert.Error(t, json.Unmarshal([]byte(tc.body[:len(tc.body)-1]+`,"unknown":true}`), &withUnknown))
		})
	}
	for _, body := range []string{
		`{"mode":"set","window_seconds":"86400","retroactive":"apply"}`,
		`{"mode":"resume","revision":"7"}`,
	} {
		var request Request
		assert.Error(t, json.Unmarshal([]byte(body), &request))
	}
}

func TestRequestJSONContract_RejectsForbiddenFieldsRegardlessOfValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"set revision null", `{"mode":"set","window_seconds":86400,"retroactive":"apply","revision":null}`},
		{"set revision zero", `{"mode":"set","window_seconds":86400,"retroactive":"apply","revision":0}`},
		{"clear window null", `{"mode":"clear","window_seconds":null,"retroactive":"clear_pending"}`},
		{"clear window zero", `{"mode":"clear","window_seconds":0,"retroactive":"clear_pending"}`},
		{"clear revision null", `{"mode":"clear","retroactive":"clear_pending","revision":null}`},
		{"clear revision zero", `{"mode":"clear","retroactive":"clear_pending","revision":0}`},
		{"resume window null", `{"mode":"resume","revision":7,"window_seconds":null}`},
		{"resume window zero", `{"mode":"resume","revision":7,"window_seconds":0}`},
		{"resume retroactive empty", `{"mode":"resume","revision":7,"retroactive":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request Request
			assert.Error(t, json.Unmarshal([]byte(tc.body), &request))
		})
	}
}

func TestRequestJSONContract_UsesExactFieldNamesAndPreservesReceiverOnError(t *testing.T) {
	window := 3600
	revision := int64(7)
	sentinel := Request{Mode: "resume", WindowSeconds: &window, Retroactive: "sentinel", Revision: &revision}
	for _, body := range []string{
		`{"Mode":"set","window_seconds":86400,"retroactive":"apply"}`,
		`{"mode":"set","Window_Seconds":86400,"retroactive":"apply"}`,
		`{"mode":"set","window_seconds":86400,"Retroactive":"apply"}`,
	} {
		received := sentinel
		assert.Error(t, json.Unmarshal([]byte(body), &received))
		assert.Equal(t, sentinel, received)
	}
}

func TestPolicyUpdate_MapsOnlyRetroactiveOperationsToMarkers(t *testing.T) {
	window := 3600
	for _, tc := range []struct {
		name       string
		request    Request
		wantWindow any
		wantMode   any
	}{
		{"apply", Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply"}, window, "apply"},
		{"new only", Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only"}, window, nil},
		{"clear pending", Request{Mode: "clear", Retroactive: "clear_pending"}, nil, "clear"},
		{"leave pending", Request{Mode: "clear", Retroactive: "leave_pending"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotWindow, gotMode := policyUpdate(tc.request)
			assert.Equal(t, tc.wantWindow, gotWindow)
			assert.Equal(t, tc.wantMode, gotMode)
		})
	}
}

func TestService_StartRejectsMissingCallerTransaction(t *testing.T) {
	service := NewService(nil)
	_, err := service.StartChannel(context.Background(), nil, "channel", Request{Mode: "clear", Retroactive: "leave_pending"})
	assert.ErrorIs(t, err, ErrServiceUnready)
}

func TestStartErrorStatus_ClassifiesWrappedPublicErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"bad request", fmt.Errorf("wrapped: %w", ErrInvalidRequest), http.StatusBadRequest},
		{"bad window", ErrInvalidWindow, http.StatusBadRequest},
		{"missing scope", fmt.Errorf("wrapped: %w", ErrScopeNotFound), http.StatusNotFound},
		{"pending", ErrBackfillPending, http.StatusConflict},
		{"missing backfill", fmt.Errorf("wrapped: %w", ErrBackfillNotFound), http.StatusConflict},
		{"revision", fmt.Errorf("wrapped: %w", ErrRevisionMismatch), http.StatusConflict},
		{"unexpected", fmt.Errorf("database: %w", context.Canceled), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, StartErrorStatus(tc.err)) })
	}
}

func TestNormalizeResume_PreservesPublicPolicyContract(t *testing.T) {
	startAt := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	resumedAt := startAt.Add(time.Second)
	startWindow := 3600
	resumedWindow := 86400
	start := Policy{WindowSeconds: &startWindow, UpdatedAt: &startAt, Revision: 4, BackfillPending: true}
	for _, tc := range []struct {
		name       string
		resumed    Policy
		err        error
		wantPolicy Policy
		wantStatus int
	}{
		{
			name:       "successful resume",
			resumed:    Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 4, BackfillPending: false},
			wantPolicy: Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 4, BackfillPending: false},
			wantStatus: http.StatusOK,
		},
		{
			name:       "sparse unexpected error falls back to pending start",
			resumed:    Policy{},
			err:        fmt.Errorf("wrapped database error: %w", context.Canceled),
			wantPolicy: Policy{WindowSeconds: &startWindow, UpdatedAt: &startAt, Revision: 4, BackfillPending: true},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "authoritative unexpected error preserves resumed policy",
			resumed:    Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			err:        context.DeadlineExceeded,
			wantPolicy: Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "wrapped revision conflict preserves authoritative policy",
			resumed:    Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			err:        fmt.Errorf("wrapped: %w", ErrRevisionMismatch),
			wantPolicy: Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "wrapped backfill conflict preserves authoritative policy",
			resumed:    Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			err:        fmt.Errorf("wrapped: %w", ErrBackfillNotFound),
			wantPolicy: Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: false},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "pending error preserves durable pending policy",
			resumed:    Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: true},
			err:        ErrBackfillPending,
			wantPolicy: Policy{WindowSeconds: &resumedWindow, UpdatedAt: &resumedAt, Revision: 9, BackfillPending: true},
			wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotPolicy, gotStatus := NormalizeResume(start, tc.resumed, tc.err)
			assert.Equal(t, tc.wantPolicy, gotPolicy)
			assert.Equal(t, tc.wantStatus, gotStatus)
		})
	}
}

func ptr[T any](value T) *T { return &value }
