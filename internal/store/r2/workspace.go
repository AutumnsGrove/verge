// Workspaces are the second kind of object this bucket holds, and the reason the
// package is named after R2 rather than after the document.
//
// The document (Store) is bookkeeping: which machines exist, who holds them,
// losing it costs a reconcile. A workspace is the work itself: losing it costs a
// person their afternoon. They share a bucket and an S3 client and nothing else,
// which is why they are two types on one API rather than one type with a mode.
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Workspace is a syncer.Sink: one object per machine, keyed by the caller.
//
// It implements the interface structurally -- it does not import syncer -- so the
// dependency runs one way: the store knows about S3, and the syncer knows about
// the store only as "something that can Put".
type Workspace struct {
	api    ObjectAPI
	bucket string
}

// WorkspaceConfig is where workspaces live.
type WorkspaceConfig struct {
	// Bucket is required.
	Bucket string

	// API talks to R2. The same *s3.Client the document store uses is the ordinary
	// value; the narrowed ObjectAPI is GetObject and PutObject, so a client handed
	// to this package cannot be talked into deleting a workspace by a code path
	// nobody reviewed.
	API ObjectAPI
}

// ErrWorkspaceKey means the key is not one this store will send.
var ErrWorkspaceKey = errors.New("r2: unusable object key")

// NewWorkspace validates the configuration and returns the store.
func NewWorkspace(cfg WorkspaceConfig) (*Workspace, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("%w: no Bucket", ErrConfig)
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("%w: no API", ErrConfig)
	}
	return &Workspace{api: cfg.API, bucket: cfg.Bucket}, nil
}

// Put stores one object, replacing any previous copy at the key.
//
// Overwriting is the design rather than an accident: a machine syncs, is kept, and
// may sync again, and the last write is the workspace worth restoring. There is no
// versioning here -- R2 buckets can have it, and that is where a history belongs,
// not in a second key scheme this package would have to invent and explain.
func (w *Workspace) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if !usableKey(key) {
		return fmt.Errorf("%w: %q", ErrWorkspaceKey, key)
	}

	// ContentLength is the caller's size -- the number it spooled -- rather than a
	// length inferred from the body. The body is seekable, so the SDK could infer
	// one; setting it makes the sink's statement about the workspace the one the
	// request is built from, and net/http refuses a request whose body disagrees
	// with it rather than storing a truncated tarball that looks saved.
	_, err := w.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(w.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/x-tar"),
	})
	if err != nil {
		return fmt.Errorf("storing %q: %w", key, err)
	}
	return nil
}

// usableKey refuses what a caller should never have produced.
//
// The syncer validates the machine id before it builds a key, so this is the
// second gate rather than the first. It exists because a key is a caller-supplied
// string in an S3 request, and "the caller validated it" is a claim that rots:
// a leading slash is a different object in some tools, an empty key is the bucket
// list, and a dot-dot segment is the shape a path-escape takes.
func usableKey(key string) bool {
	if key == "" {
		return false
	}
	for _, elem := range strings.Split(key, "/") {
		if elem == "" || elem == "." || elem == ".." || strings.ContainsRune(elem, '\\') {
			return false
		}
	}
	return true
}
