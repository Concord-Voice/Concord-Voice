package feedback

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateIssue_Success verifies the happy path: a 201 from a synthetic
// GitHub server is round-tripped to the caller as an IssueResponse.
func TestCreateIssue_Success(t *testing.T) {
	var capturedAuth, capturedAPIVersion, capturedAccept, capturedUA string
	var capturedBody IssueRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/Concord-Voice/Concord-Voice-Feedback/issues", r.URL.Path)
		capturedAuth = r.Header.Get("Authorization")
		capturedAPIVersion = r.Header.Get("X-GitHub-Api-Version")
		capturedAccept = r.Header.Get("Accept")
		capturedUA = r.Header.Get("User-Agent")
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &capturedBody))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/foo/bar/issues/42"}`))
	}))
	defer srv.Close()

	client := NewClient("test-pat", "Concord-Voice/Concord-Voice-Feedback")
	client.baseURL = srv.URL

	issue, err := client.CreateIssue(context.Background(), IssueRequest{
		Title:  "[Bug Report] Test",
		Body:   "Description goes here",
		Labels: []string{"type: bug"},
	})

	require.NoError(t, err)
	assert.Equal(t, 42, issue.Number)
	assert.Equal(t, "https://github.com/foo/bar/issues/42", issue.HTMLURL)

	// Headers
	assert.Equal(t, "Bearer test-pat", capturedAuth)
	assert.Equal(t, "2022-11-28", capturedAPIVersion)
	assert.Equal(t, "application/vnd.github+json", capturedAccept)
	assert.Equal(t, "concord-feedback-bot", capturedUA)

	// Body shape
	assert.Equal(t, "[Bug Report] Test", capturedBody.Title)
	assert.Equal(t, "Description goes here", capturedBody.Body)
	assert.Equal(t, []string{"type: bug"}, capturedBody.Labels)
}

// TestCreateIssue_Non2xxReturnsError ensures GitHub error responses surface
// as a Go error with the status code and a (truncated) body excerpt.
func TestCreateIssue_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Issue"}]}`))
	}))
	defer srv.Close()

	client := NewClient("test-pat", "owner/repo")
	client.baseURL = srv.URL

	_, err := client.CreateIssue(context.Background(), IssueRequest{Title: "t", Body: "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github: API status 422")
	assert.Contains(t, err.Error(), "Validation Failed")
}

// TestCreateIssue_TransportError covers the "GitHub unreachable" case via
// pointing the client at an unused port on localhost.
func TestCreateIssue_TransportError(t *testing.T) {
	client := NewClient("test-pat", "owner/repo")
	client.baseURL = "http://127.0.0.1:1" // port 1 — guaranteed-not-listening

	_, err := client.CreateIssue(context.Background(), IssueRequest{Title: "t", Body: "b"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "github: transport:"),
		"expected transport-error wrapping, got: %v", err)
}

// TestCreateIssue_LabelsOmittedWhenEmpty ensures the omitempty on Labels works —
// a request with no labels should NOT serialize `"labels":[]` (cosmetic, but
// keeps the rendered issue clean of empty arrays).
func TestCreateIssue_LabelsOmittedWhenEmpty(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":1,"html_url":"x"}`))
	}))
	defer srv.Close()

	client := NewClient("p", "o/r")
	client.baseURL = srv.URL
	_, _ = client.CreateIssue(context.Background(), IssueRequest{Title: "t", Body: "b"})
	assert.NotContains(t, receivedBody, "labels")
}
