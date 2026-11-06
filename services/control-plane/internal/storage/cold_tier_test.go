package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

// fakeS3 answers just enough of the S3 API for a GET (with a storage class)
// and an in-place CopyObject, and records the copy request's headers.
func fakeS3(t *testing.T, copied *http.Header) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "3")
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("ETag", `"e"`)
			w.Header().Set("x-amz-storage-class", "STANDARD_IA")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("abc"))
			}
		case http.MethodPut:
			*copied = r.Header.Clone()
			_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"e"</ETag><LastModified>2026-11-06T00:00:00.000Z</LastModified></CopyObjectResult>`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	core, err := minio.NewCore(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4("ak", "sk", ""), Region: "auto",
	})
	require.NoError(t, err)
	return &Client{minio: core, bucket: "bkt"}
}

func TestGetObjectWithClass_ReportsStorageClass(t *testing.T) {
	var copied http.Header
	c := fakeS3(t, &copied)
	obj, _, class, err := c.GetObjectWithClass(context.Background(), "attachments/x")
	require.NoError(t, err)
	_ = obj.Close()
	require.Equal(t, StorageClassInfrequentAccess, class)
}

func TestPromoteToStandard_CopiesInPlaceKeepingMetadata(t *testing.T) {
	var copied http.Header
	c := fakeS3(t, &copied)
	require.NoError(t, c.PromoteToStandard(context.Background(), "attachments/x"))
	require.Equal(t, "STANDARD", copied.Get("x-amz-storage-class"))
	require.Equal(t, "bkt/attachments/x", copied.Get("x-amz-copy-source"))
	// REPLACE would drop the stored Content-Type; the default COPY keeps it.
	require.NotEqual(t, "REPLACE", copied.Get("x-amz-metadata-directive"))
}

func TestStorageClass_FromHead(t *testing.T) {
	var copied http.Header
	c := fakeS3(t, &copied)
	class, err := c.StorageClass(context.Background(), "attachments/x")
	require.NoError(t, err)
	require.Equal(t, StorageClassInfrequentAccess, class)
}
