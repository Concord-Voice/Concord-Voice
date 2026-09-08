package admin_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

type errorCloser struct{ err error }

func (c errorCloser) Close() error { return c.err }

func TestRunAdminCtlWithWriterForTestFailsBeforeMutationWhenWriterOpenFails(t *testing.T) {
	var output bytes.Buffer
	code := admin.RunAdminCtlWithWriterForTest(context.Background(), nil, nil, nil, &output, "https://admin.test", []string{"bootstrap"}, func() (securityevent.Emitter, io.Closer, error) {
		return nil, nil, errors.New("writer unavailable")
	})
	require.Equal(t, 1, code)
	require.Contains(t, output.String(), "open security events")
}

func TestRunAdminCtlWithWriterForTestReportsCloseFailure(t *testing.T) {
	db, dbCleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(dbCleanup)
	rdb, redisCleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(redisCleanup)

	var output bytes.Buffer
	closeErr := errors.New("writer close failed")
	code := admin.RunAdminCtlWithWriterForTest(
		context.Background(), db, rdb,
		strings.NewReader("Str0ng-P@ssw0rd-123\n"), &output, "https://admin.test",
		[]string{"bootstrap", "--username", uniqueAdminUsername("close-failure"), "--password-stdin"},
		func() (securityevent.Emitter, io.Closer, error) {
			return securityevent.Discard, errorCloser{err: closeErr}, nil
		},
	)

	require.Equal(t, 1, code)
	require.Contains(t, output.String(), "admin: close security events: writer close failed")
}
