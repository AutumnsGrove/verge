//go:build contract

package r2_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/store/r2"
)

// The real bucket, round-tripping a document under a key of its own.
//
// The key carries a per-run suffix rather than being a fixed name, because two runs
// at once -- a developer and CI, or two CI jobs -- would otherwise read back each
// other's document and call it a round trip. Nothing is deleted, since the narrowed
// ObjectAPI has no DeleteObject and a test that tidied up by deleting is a test whose
// tidying-up is the risk; the run therefore leaves one small object, and the bucket
// this uses wants a lifecycle rule rather than a cleanup routine.
//
// Missing configuration skips with the reason named (docs/testing.md).

// store returns the real store, or skips and says what is missing.
func store(t *testing.T) *r2.Store {
	t.Helper()

	env := map[string]string{
		"VERGE_S3_ACCESS_KEY": os.Getenv("VERGE_S3_ACCESS_KEY"),
		"VERGE_S3_SECRET_KEY": os.Getenv("VERGE_S3_SECRET_KEY"),
		"VERGE_R2_ENDPOINT":   os.Getenv("VERGE_R2_ENDPOINT"),
		"VERGE_R2_BUCKET":     os.Getenv("VERGE_R2_BUCKET"),
	}
	for _, name := range []string{"VERGE_S3_ACCESS_KEY", "VERGE_S3_SECRET_KEY", "VERGE_R2_ENDPOINT", "VERGE_R2_BUCKET"} {
		if env[name] == "" {
			t.Skipf("contract: %s is not set, so there is no bucket to round-trip", name)
		}
	}

	api := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(env["VERGE_S3_ACCESS_KEY"], env["VERGE_S3_SECRET_KEY"], ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(env["VERGE_R2_ENDPOINT"])
		o.UsePathStyle = true
	})

	key := fmt.Sprintf("contract-state-%d.json", time.Now().UnixNano())
	s, err := r2.New(r2.Config{Bucket: env["VERGE_R2_BUCKET"], Key: key, API: api})
	if err != nil {
		t.Fatalf("r2.New: %v", err)
	}
	return s
}

// TestContractRoundTrip is the store's whole contract against the real bucket: a
// document written is a document read back, with every field the store is responsible
// for preserved. The three failure modes the unit tier distinguishes -- absent,
// broken, forbidden -- are not repeated here, because they cannot be provoked without
// hurting a real bucket.
func TestContractRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s := store(t)

	// Every field the document has -- a machine with its Error record populated, and
	// a lease with its own instants -- because a round trip that preserved only the
	// name and the state would pass the assertions this replaces, and the fields it
	// skipped are the ones a restart rebuilds its decisions from. The lease is the
	// half that keeps a machine alive across that restart.
	base := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	doc := firefly.Document{
		Version:           firefly.DocumentVersion,
		NextLeaseNumber:   7,
		NextMachineNumber: 3,
		Machines: []firefly.Machine{{
			ID:           firefly.MachineID("hetzner:99999999999"),
			Name:         "verge-contract-1",
			State:        firefly.Error,
			Origin:       firefly.Syncing,
			CreatedAt:    base,
			Deadline:     base.Add(4 * time.Hour),
			Since:        base.Add(90 * time.Minute),
			ReachedReady: true,
			Failures:     3,
			Attempt:      2,
			LastErr:      "the save could not succeed",
			RetryAt:      base.Add(95 * time.Minute),
		}},
		Leases: []firefly.Lease{{
			ID:      firefly.LeaseID("lease-1"),
			Machine: firefly.MachineID("hetzner:99999999999"),
			Holder:  "session:01J8CONTRACT",
			Kind:    firefly.Session,
			Start:   base.Add(30 * time.Minute),
			TTL:     45 * time.Minute,
		}},
	}
	if err := s.Save(ctx, doc); err != nil {
		t.Fatalf("Save against the real bucket: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load against the real bucket: %v", err)
	}
	if want, have := normalise(doc), normalise(got); !reflect.DeepEqual(have, want) {
		t.Fatalf("document round trip:\n got %+v\nwant %+v", have, want)
	}
}

// normalise puts every instant in UTC so a comparison can be about the instant
// rather than about which location a JSON decoder chose. It is the only reason this
// file mentions DeepEqual: every other field must match exactly, which is the point.
func normalise(doc firefly.Document) firefly.Document {
	for i := range doc.Machines {
		m := &doc.Machines[i]
		m.CreatedAt = m.CreatedAt.UTC()
		m.Deadline = m.Deadline.UTC()
		m.Since = m.Since.UTC()
		m.RetryAt = m.RetryAt.UTC()
	}
	for i := range doc.Leases {
		doc.Leases[i].Start = doc.Leases[i].Start.UTC()
	}
	return doc
}
