package r2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/AutumnsGrove/verge/internal/syncer"
)

// The workspace store is a syncer.Sink, stated where a compile error is cheap.
var _ syncer.Sink = (*Workspace)(nil)

// received is one request the fake bucket saw.
type received struct {
	method      string
	path        string
	contentType string
	length      string
	body        []byte
}

// newWorkspace points a real S3 client at an httptest server, the same way the
// document store's tests do: the seam under test is HTTP, so nothing is mocked.
func newWorkspace(t *testing.T, records *[]received, status int) *Workspace {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*records = append(*records, received{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			length:      r.Header.Get("Content-Length"),
			body:        b,
		})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
		HTTPClient:  srv.Client(),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})

	ws, err := NewWorkspace(WorkspaceConfig{Bucket: testBucket, API: client})
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

func TestWorkspacePut(t *testing.T) {
	t.Run("happy/stores_the_object_at_its_key", func(t *testing.T) {
		var got []received
		ws := newWorkspace(t, &got, http.StatusOK)

		body := []byte("a tarball, honestly")
		if err := ws.Put(context.Background(), "state/fake-001.tar", bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("Put: %v", err)
		}

		if len(got) != 1 {
			t.Fatalf("the bucket saw %d requests, want 1", len(got))
		}
		if got[0].method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", got[0].method)
		}
		if want := "/" + testBucket + "/state/fake-001.tar"; got[0].path != want {
			t.Fatalf("path = %q, want %q", got[0].path, want)
		}
		if !bytes.Equal(got[0].body, body) {
			t.Fatalf("body = %q, want the tarball", got[0].body)
		}
		if got[0].contentType != "application/x-tar" {
			t.Fatalf("content type = %q, want application/x-tar", got[0].contentType)
		}
		if want := strconv.Itoa(len(body)); got[0].length != want {
			t.Fatalf("content length = %q, want %s", got[0].length, want)
		}
	})

	t.Run("happy/one_machine_can_be_stored_twice", func(t *testing.T) {
		// Overwriting is the resting state of this design -- a machine syncs, is
		// kept, and syncs again -- so a second PUT to the same key must be ordinary.
		var got []received
		ws := newWorkspace(t, &got, http.StatusOK)

		for _, body := range [][]byte{[]byte("first"), []byte("second")} {
			if err := ws.Put(context.Background(), "state/fake-001.tar", bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if len(got) != 2 || got[0].path != got[1].path {
			t.Fatalf("paths = %q, %q, want the same key twice", pathOf(got, 0), pathOf(got, 1))
		}
	})

	t.Run("sad/bucket_refuses", func(t *testing.T) {
		var got []received
		ws := newWorkspace(t, &got, http.StatusForbidden)

		err := ws.Put(context.Background(), "state/fake-001.tar", strings.NewReader("x"), 1)
		if err == nil {
			t.Fatal("Put = nil, want the bucket's refusal: a lost workspace must not look saved")
		}
		if !strings.Contains(err.Error(), "storing") {
			t.Fatalf("Put = %v, want it to name the operation", err)
		}
	})

	t.Run("bad/size_disagrees_with_the_body", func(t *testing.T) {
		// The size is the caller's statement about the workspace, and it is what the
		// request is built from. A body that disagrees with it is refused instead of
		// being stored at whichever of the two lengths happened to win -- and that is
		// what makes the ContentLength line load-bearing rather than decorative.
		var got []received
		ws := newWorkspace(t, &got, http.StatusOK)

		err := ws.Put(context.Background(), "state/fake-001.tar", bytes.NewReader(make([]byte, 10)), 7)
		if err == nil {
			t.Fatal("Put with a size that disagrees with the body = nil, want an error")
		}
	})

	t.Run("bad/empty_key", func(t *testing.T) {
		var got []received
		ws := newWorkspace(t, &got, http.StatusOK)

		if err := ws.Put(context.Background(), "", strings.NewReader("x"), 1); err == nil {
			t.Fatal("Put with an empty key = nil, want a refusal")
		}
		if len(got) != 0 {
			t.Fatalf("the bucket saw %d requests for an empty key, want 0", len(got))
		}
	})
}

// TestUsableKey covers every branch of the second gate, one case each: the guard
// is only worth having if a deleted branch is a failing test.
func TestUsableKey(t *testing.T) {
	for _, key := range []string{"state/fake-001.tar", "verge/prod/x.tar", "a.b-c_d", "a/b/c"} {
		if !usableKey(key) {
			t.Fatalf("usableKey(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"", "/x", "a/../b", "a//b", `a\b`, "state/./x", "..", "a/", "."} {
		if usableKey(key) {
			t.Fatalf("usableKey(%q) = true, want false", key)
		}
	}
}

func pathOf(got []received, i int) string {
	if i >= len(got) {
		return ""
	}
	return got[i].path
}

func TestNewWorkspace(t *testing.T) {
	t.Run("bad/no_bucket", func(t *testing.T) {
		if _, err := NewWorkspace(WorkspaceConfig{}); !errors.Is(err, ErrConfig) {
			t.Fatalf("NewWorkspace = %v, want %v", err, ErrConfig)
		}
	})

	t.Run("bad/no_api", func(t *testing.T) {
		if _, err := NewWorkspace(WorkspaceConfig{Bucket: testBucket}); !errors.Is(err, ErrConfig) {
			t.Fatalf("NewWorkspace = %v, want %v", err, ErrConfig)
		}
	})
}
