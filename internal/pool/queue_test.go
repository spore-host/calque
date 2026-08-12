package pool

import (
	"context"
	"testing"
)

// ---- CreateRunQueue/OpenRunQueue/DeleteRunQueueIfExists (calque#145) -------
//
// fakeSQS (defined in provision_test.go, same package) already covers every
// SQS operation these need — CreateQueue/GetQueueUrl/DeleteQueue — so these
// tests reuse it directly rather than defining a second fake.

// TestCreateRunQueue_ThenOpenRunQueueResolvesSameQueue proves the
// submitter/worker round trip: a queue CreateRunQueue creates for a run id
// must be resolvable by OpenRunQueue using ONLY that same run id — the
// worker-boots-knowing-only-its-run-id path calque#145's fleet workers need,
// mirroring CreatePoolQueue/OpenPoolQueue's identical contract.
func TestCreateRunQueue_ThenOpenRunQueueResolvesSameQueue(t *testing.T) {
	sqsc := newFakeSQS()
	ctx := context.Background()

	created, err := CreateRunQueue(ctx, sqsc, "run-1", 900)
	if err != nil {
		t.Fatalf("CreateRunQueue: %v", err)
	}

	opened, err := OpenRunQueue(ctx, sqsc, "run-1")
	if err != nil {
		t.Fatalf("OpenRunQueue: %v", err)
	}
	if opened.URL() != created.URL() {
		t.Errorf("OpenRunQueue resolved %q, want the same queue CreateRunQueue made (%q)", opened.URL(), created.URL())
	}
}

// TestOpenRunQueue_DoesNotResolveAPoolQueue proves the two queue kinds are
// genuinely isolated by name — a run queue and a pool queue sharing an
// identifier (e.g. a run id that happens to equal some pool's model name)
// must never be confused with each other, since RunQueueName/PoolQueueName
// use disjoint prefixes. Checked at the name level (not by actually calling
// OpenRunQueue against a pool-only queue) to avoid exercising
// OpenRunQueue's real multi-attempt eventual-consistency retry loop, which
// would make this test slow for no additional coverage — the retry loop
// itself is exercised elsewhere in this package's existing tests.
func TestOpenRunQueue_DoesNotResolveAPoolQueue(t *testing.T) {
	if RunQueueName("shared-name") == PoolQueueName("shared-name") {
		t.Fatal("RunQueueName and PoolQueueName must not collide for the same identifier")
	}
}

// TestDeleteRunQueueIfExists_DeletesALiveQueue proves the delete half
// actually removes a queue that exists.
func TestDeleteRunQueueIfExists_DeletesALiveQueue(t *testing.T) {
	sqsc := newFakeSQS()
	ctx := context.Background()

	if _, err := CreateRunQueue(ctx, sqsc, "run-2", 900); err != nil {
		t.Fatalf("CreateRunQueue: %v", err)
	}
	if err := DeleteRunQueueIfExists(ctx, sqsc, "run-2"); err != nil {
		t.Fatalf("DeleteRunQueueIfExists: %v", err)
	}
	if !sqsc.queueDeleted(RunQueueName("run-2")) {
		t.Error("DeleteRunQueueIfExists did not actually delete the queue")
	}
}

// TestDeleteRunQueueIfExists_NoQueueIsNotAnError mirrors
// TestDeletePool_NoQueueIsNotAnError: a second teardown call (or a run whose
// queue was never created) must succeed idempotently, not fail.
func TestDeleteRunQueueIfExists_NoQueueIsNotAnError(t *testing.T) {
	sqsc := newFakeSQS() // no queue seeded at all
	if err := DeleteRunQueueIfExists(context.Background(), sqsc, "run-3"); err != nil {
		t.Fatalf("DeleteRunQueueIfExists with no pre-existing queue should succeed, got: %v", err)
	}
}
