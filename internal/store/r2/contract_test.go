//go:build contract

package r2_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/store/r2"
)

// The real bucket, round-tripping a document under a key of its own: overwriting
// "contract-state.json" each run is the whole cleanup, because the narrowed
// ObjectAPI has no DeleteObject and a contract test that tidied up by deleting is a
// contract test whose tidying-up is the risk.
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

	s, err := r2.New(r2.Config{Bucket: env["VERGE_R2_BUCKET"], Key: "contract-state.json", API: api})
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

	doc := firefly.Document{
		Version:           firefly.DocumentVersion,
		NextLeaseNumber:   1,
		NextMachineNumber: 2,
		Machines: []firefly.Machine{{
			ID:           firefly.MachineID("hetzner:99999999999"),
			Name:         "verge-contract-1",
			State:        firefly.Ready,
			CreatedAt:    time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
			Deadline:     time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC),
			Since:        time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
			ReachedReady: true,
		}},
	}
	if err := s.Save(ctx, doc); err != nil {
		t.Fatalf("Save against the real bucket: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load against the real bucket: %v", err)
	}
	if got.Version != doc.Version || got.NextLeaseNumber != doc.NextLeaseNumber || got.NextMachineNumber != doc.NextMachineNumber {
		t.Fatalf("document round trip lost bookkeeping: got %+v, want %+v", got, doc)
	}
	if len(got.Machines) != 1 || got.Machines[0].Name != doc.Machines[0].Name || got.Machines[0].State != doc.Machines[0].State {
		t.Fatalf("document round trip lost the machine: got %+v", got.Machines)
	}
}
