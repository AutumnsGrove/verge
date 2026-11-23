// Package r2 is the real Store: the state document as one JSON object in
// Cloudflare R2, which speaks the S3 API.
//
// It exists because MemStore is test-only. Without it the document dies with the
// process, which means every machine firefly owns becomes an orphan on restart —
// recoverable only by adoption, and only if the listing happens to be complete.
//
// The surface is two calls. `Store` is whole-document on both read and write
// (docs/firefly.md, open question 1), so a partial update cannot record a machine as
// terminating while its lease still holds it open. There is one object, one key, and
// no listing: the document is not a directory.
package r2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// maxDocumentBytes bounds what Load will read.
//
// A document is machines plus leases: a few kilobytes for any deployment that fits
// on a bill. The limit is here because a store that reads until EOF from a source it
// did not write is a store that allocates whatever it is handed, and the failure it
// prevents — a truncated response looping forever — is one that costs memory rather
// than time.
const maxDocumentBytes = 8 << 20

// ObjectAPI is the part of the S3 client this package uses.
//
// *s3.Client satisfies it. It is narrowed rather than accepted whole for one reason:
// a store with access to `DeleteObject` and bucket listings is a store that will one
// day be used to tidy up, and the state document has exactly one object in it that
// must never be removed by a code path nobody reviewed.
type ObjectAPI interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Config is where the document lives.
type Config struct {
	// Bucket and Key name the object. Both are required.
	//
	// The key is a plain object key, so a deployment can namespace it ("verge/prod.json")
	// without this package knowing what a deployment is.
	Bucket string
	Key    string

	// API talks to R2. *s3.Client is the ordinary value; a caller builds it with
	// the account's S3 endpoint, region "auto", and credentials that can only touch
	// this bucket.
	API ObjectAPI
}

// Store is firefly's state document in one object.
type Store struct {
	api    ObjectAPI
	bucket string
	key    string
}

// ErrConfig means the store cannot be built from its configuration.
var ErrConfig = errors.New("r2: invalid configuration")

// New validates the configuration and returns the store.
//
// Nothing is read or written here: a store that dialled out at construction could not
// be built inside an offline test tier, and firefly's first load is where an
// unreachable bucket belongs — the same reason firefly.New touches no storage.
func New(cfg Config) (*Store, error) {
	switch {
	case cfg.API == nil:
		return nil, fmt.Errorf("%w: no API", ErrConfig)
	case cfg.Bucket == "":
		return nil, fmt.Errorf("%w: no Bucket", ErrConfig)
	case cfg.Key == "":
		return nil, fmt.Errorf("%w: no Key", ErrConfig)
	}
	return &Store{api: cfg.API, bucket: cfg.Bucket, key: cfg.Key}, nil
}

var _ firefly.Store = (*Store)(nil)

// Load reads the document.
//
// A missing object is ErrNoDocument and not an error, because a fresh deployment has
// no state and firefly treats that as an empty document rather than a reason to stop.
// Everything else — a forbidden read, a truncated body, a body that is not the
// document — is an error, and deliberately not ErrNoDocument: a store that cannot be
// read is not an empty store, and treating it as one would let a restart adopt nothing
// and delete everything.
//
// The document is returned as stored. Validating it is firefly's job, on both paths,
// so that the refusal is the same whichever store is behind it.
func (s *Store) Load(ctx context.Context) (firefly.Document, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return firefly.Document{}, firefly.ErrNoDocument
		}
		return firefly.Document{}, fmt.Errorf("r2: reading %s/%s: %w", s.bucket, s.key, err)
	}
	defer out.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(out.Body, maxDocumentBytes+1))
	if err != nil {
		return firefly.Document{}, fmt.Errorf("r2: reading %s/%s: %w", s.bucket, s.key, err)
	}
	if len(raw) > maxDocumentBytes {
		return firefly.Document{}, fmt.Errorf("r2: %s/%s is larger than %d bytes", s.bucket, s.key, maxDocumentBytes)
	}

	var doc firefly.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return firefly.Document{}, fmt.Errorf("r2: decoding %s/%s: %w", s.bucket, s.key, err)
	}
	return doc, nil
}

// Save replaces the document.
//
// One PUT of the whole object, which R2 applies atomically: a reader either sees the
// previous document or this one, never a half-written mixture. There is no conditional
// write, so two orchestrators pointing at one key would overwrite each other — that is
// open question 5 in docs/firefly.md (adoption after a split brain), and it is not a
// question this package should answer by silently losing writes.
func (s *Store) Save(ctx context.Context, doc firefly.Document) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("r2: encoding the document: %w", err)
	}

	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key),
		Body:          bytes.NewReader(raw),
		ContentLength: aws.Int64(int64(len(raw))),
		ContentType:   aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("r2: writing %s/%s: %w", s.bucket, s.key, err)
	}
	return nil
}

// isNoSuchKey reports whether err is S3 saying the object is not there.
//
// NoSuchBucket is deliberately not included: a missing bucket is a deployment that
// has been misconfigured or torn down, not a fresh one, and reading it as "no document
// yet" would make the first save create a bucket-shaped error instead of the first
// load failing where an operator can see it.
func isNoSuchKey(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchKey"
}
