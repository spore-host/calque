package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithy "github.com/aws/smithy-go"

	"github.com/spore-host/calque/internal/calquetags"
	"github.com/spore-host/calque/pkg/calquefootprint"
	"github.com/spore-host/spawn/pkg/taskpool"
)

// SQSAPI is the slice of the SQS SDK pool.SQSQueue needs: taskpool.SQSAPI's
// own Claim/Ack-supporting methods, plus ChangeMessageVisibility for
// calque#131's heartbeat extension — which taskpool.SQSAPI does not declare
// (spawn's fungible, short-lived CPU tasks have no heartbeat need). A real
// *sqs.Client satisfies both this and taskpool.SQSAPI unchanged.
//
// ListQueueTags and TagQueue (calque#227) bring an existing queue's tags up
// to date; see createTaggedQueue.
type SQSAPI interface {
	taskpool.SQSAPI
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	ListQueueTags(ctx context.Context, in *sqs.ListQueueTagsInput, opts ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
	TagQueue(ctx context.Context, in *sqs.TagQueueInput, opts ...func(*sqs.Options)) (*sqs.TagQueueOutput, error)
}

// SQSQueue is the production pool.Queue: an SQS-backed adapter reusing spawn's
// taskpool.SQSAPI interface (the same ReceiveMessage/DeleteMessage/etc. shape,
// so a real *sqs.Client satisfies both) per docs/pool-queue-contract.md's
// "reuse the identical SQS Claim/Ack semantics" decision.
//
// It does NOT reuse taskpool.Queue/CreateQueue/OpenQueue directly: those are
// hardcoded to a RUN-scoped name ("spawn-pool-"+runID) with a 12h
// MessageRetentionPeriod sized for "a leaked queue still expires... far longer
// than any interactive fan-out" (taskpool/queue.go). A calque pool queue is the
// opposite: it's MODEL-scoped and outlives any one run for the pool's whole
// lifetime (decision 2 of docs/pool-queue-contract.md) — forcing a 12h expiry
// on it would silently kill a long-idle-but-still-valid pool. So this type
// mirrors taskpool's Claim/Ack/CreateQueue/OpenQueue LOGIC (idempotent create,
// eventual-consistency retry on GetQueueUrl) against calque's own naming and
// SQS's default 4-day message retention (poolQueueRetentionSeconds), rather
// than importing the run-scoped type verbatim.
type SQSQueue struct {
	client SQSAPI
	url    string
}

var _ Queue = (*SQSQueue)(nil)

// poolQueueRetentionSeconds is a pool queue's MessageRetentionPeriod: 4
// days, which is SQS's own default, now stated rather than implied
// (calque#227). It bounds how long an unclaimed claim ref waits; it does NOT
// expire the queue. SQS has no queue expiry: retention drops MESSAGES, and an
// idle pool queue lives until `calque pool delete` (or calque#228's
// teardown, which finds it by its spore-host:managed-by=calque tag).
//
// It must stay the SQS default. CreateQueue on an existing name succeeds only
// when every attribute it is given matches the queue's, and pool queues made
// before calque#227 have the default, so any other value would make
// `calque pool create` fail on every pool that already exists.
const poolQueueRetentionSeconds = "345600"

// CreatePoolQueue creates (idempotently) the queue for a named model pool.
// visibilityTimeout must exceed the longest expected single-claim drain time,
// or a slow claim gets redelivered and double-run while still in flight —
// same sizing rule as taskpool.CreateQueue.
//
// The queue is tagged PoolQueueTags(model) (calque#227); see
// createTaggedQueue.
func CreatePoolQueue(ctx context.Context, client SQSAPI, model string, visibilityTimeout int) (*SQSQueue, error) {
	name := PoolQueueName(model)
	return createTaggedQueue(ctx, client, name, map[string]string{
		"VisibilityTimeout": fmt.Sprintf("%d", visibilityTimeout),
		// The lifetime of the queue itself is "as long as the pool exists",
		// ended by an explicit `calque pool delete`, not time; see
		// poolQueueRetentionSeconds.
		"MessageRetentionPeriod": poolQueueRetentionSeconds,
	}, PoolQueueTags(model, time.Now()))
}

// PoolQueueTags is the tag set of a pool's queue (calque#227): the
// calque/spore-host set with scope pool:<model>, plus calque:pool-model.
func PoolQueueTags(model string, now time.Time) map[string]string {
	m := calquetags.For(calquetags.PoolScope(model), now)
	m[calquetags.PoolModel] = calquetags.Value(model)
	return m
}

// RunQueueTags is the tag set of a fleet run's queue (calque#227): the
// calque/spore-host set with scope run:<runID>, plus calque:fleet-run and
// calque:run-id.
func RunQueueTags(runID string, now time.Time) map[string]string {
	m := calquetags.ForRun(runID, now)
	m[calquetags.FleetRun] = calquetags.Value(runID)
	return m
}

// createTaggedQueue creates (idempotently) the named queue with attrs, tagged
// want at create (calque#227), and returns a handle to it.
//
// Tagging at create needs sqs:TagQueue as well as sqs:CreateQueue. Like
// spawn's taskpool.CreateQueue, a caller without it gets an untagged queue
// and a warning rather than a failed pool or fleet run.
//
// CreateQueue on an existing name returns that queue without applying the
// tags it was given, so a queue an older calque created stays untagged by
// it. (SQS documents only attributes as having to match an existing
// queue's. In case tags count too, a QueueAlreadyExists on a tagged create is
// retried once untagged; if that fails as well, the attributes really differ
// and the error is returned.) The queue's tags are therefore read back and
// anything missing or wrong is added with TagQueue (calquetags.Converge,
// which also keeps the queue's first spore-host:created). That costs one ListQueueTags per create
// and, for a queue already tagged, nothing else. Failing to read or write
// tags is a warning: the queue works either way.
func createTaggedQueue(ctx context.Context, client SQSAPI, name string, attrs, want map[string]string) (*SQSQueue, error) {
	in := &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs, Tags: maps.Clone(want)}
	out, err := client.CreateQueue(ctx, in)
	if err != nil && calquetags.IsAccessDenied(err) {
		_, _ = fmt.Fprintf(calquetags.Warn, "[tags] WARNING: creating queue %s tagged was refused (needs sqs:TagQueue); "+
			"creating it untagged, so calque's cleanup will not recognise it: %v\n", name, err)
		in.Tags = nil
		out, err = client.CreateQueue(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("create queue %s: %w", name, err)
		}
		return &SQSQueue{client: client, url: aws.ToString(out.QueueUrl)}, nil
	}
	if err != nil && isQueueAlreadyExists(err) {
		in.Tags = nil
		out, err = client.CreateQueue(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("create queue %s: %w", name, err)
	}
	url := aws.ToString(out.QueueUrl)
	convergeQueueTags(ctx, client, name, url, want)
	return &SQSQueue{client: client, url: url}, nil
}

func convergeQueueTags(ctx context.Context, client SQSAPI, name, url string, want map[string]string) {
	have, err := client.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
	if err != nil {
		_, _ = fmt.Fprintf(calquetags.Warn, "[tags] WARNING: could not read queue %s's tags (needs sqs:ListQueueTags), "+
			"so an untagged queue from an older calque stays untagged: %v\n", name, err)
		return
	}
	add := calquetags.Converge(have.Tags, want)
	if len(add) == 0 {
		return
	}
	if _, err := client.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: aws.String(url), Tags: add}); err != nil {
		_, _ = fmt.Fprintf(calquetags.Warn, "[tags] WARNING: could not tag queue %s (needs sqs:TagQueue); "+
			"calque's cleanup will not recognise it: %v\n", name, err)
	}
}

// poolOpenRetryDelay mirrors taskpool's openQueueRetryDelay — a package var so
// tests can shrink it.
var poolOpenRetryDelay = 3 * time.Second

const poolOpenMaxAttempts = 10

// OpenPoolQueue resolves an EXISTING pool queue by model name (the worker
// path: a worker boots knowing only its pool's model, per
// docs/pool-queue-contract.md decision 3). Retries GetQueueUrl's
// eventual-consistency window after a fresh CreatePoolQueue, mirroring
// taskpool.OpenQueue's own retry loop exactly.
func OpenPoolQueue(ctx context.Context, client SQSAPI, model string) (*SQSQueue, error) {
	name := PoolQueueName(model)
	var lastErr error
	for attempt := 0; attempt < poolOpenMaxAttempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(poolOpenRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err == nil {
			return &SQSQueue{client: client, url: aws.ToString(out.QueueUrl)}, nil
		}
		lastErr = err
		if !isNonExistentQueue(err) {
			return nil, fmt.Errorf("resolve pool queue %s: %w", name, err)
		}
	}
	return nil, fmt.Errorf("resolve pool queue %s: not visible after %d attempts: %w", name, poolOpenMaxAttempts, lastErr)
}

// isQueueAlreadyExists recognises CreateQueue's refusal for an existing name
// whose attributes differ from the request's.
func isQueueAlreadyExists(err error) bool {
	var qne *types.QueueNameExists
	if errors.As(err, &qne) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "QueueAlreadyExists"
}

func isNonExistentQueue(err error) bool { return calquefootprint.IsQueueNotFound(err) }

// Claim receives up to one message, long-polling for waitSeconds (0..20).
// Implements pool.Queue. A malformed body is dropped (deleted) immediately,
// mirroring taskpool.Queue.Claim's handling, so it can't loop forever.
func (q *SQSQueue) Claim(ctx context.Context, waitSeconds int32) (ClaimRef, string, bool, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.url),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     waitSeconds,
	})
	if err != nil {
		return ClaimRef{}, "", false, fmt.Errorf("claim: %w", err)
	}
	if len(out.Messages) == 0 {
		return ClaimRef{}, "", false, nil
	}
	msg := out.Messages[0]
	var ref ClaimRef
	if err := json.Unmarshal([]byte(aws.ToString(msg.Body)), &ref); err != nil {
		_, _ = q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: aws.String(q.url), ReceiptHandle: msg.ReceiptHandle,
		})
		return ClaimRef{}, "", false, fmt.Errorf("claim: malformed ref (dropped): %w", err)
	}
	return ref, aws.ToString(msg.ReceiptHandle), true, nil
}

// Ack deletes a claimed message. Implements pool.Queue.
func (q *SQSQueue) Ack(ctx context.Context, receiptHandle string) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.url),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	return nil
}

// Submit enqueues one claim ref — the submitter side (calque#103's --pool
// flag), included here since it's the natural counterpart to Claim/Ack on the
// same queue and needs no separate type.
func (q *SQSQueue) Submit(ctx context.Context, ref ClaimRef) error {
	body, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("marshal claim ref for run %s: %w", ref.RunID, err)
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.url),
		MessageBody: aws.String(string(body)),
	})
	if err != nil {
		return fmt.Errorf("submit claim for run %s: %w", ref.RunID, err)
	}
	return nil
}

// URL returns the queue's SQS URL.
func (q *SQSQueue) URL() string { return q.url }

// Extend resets a claimed message's visibility timeout to timeoutSeconds from
// now, via SQS ChangeMessageVisibility. Implements pool.Queue — calque#131's
// heartbeat hook, called periodically by Worker.runOne while a claim's batch
// is still draining, so a slow DrainBatch doesn't outlive the queue's
// visibility window and get redelivered/double-run while still in flight.
func (q *SQSQueue) Extend(ctx context.Context, receiptHandle string, timeoutSeconds int) error {
	_, err := q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(q.url),
		ReceiptHandle:     aws.String(receiptHandle),
		VisibilityTimeout: int32(timeoutSeconds),
	})
	if err != nil {
		return fmt.Errorf("extend visibility: %w", err)
	}
	return nil
}

// Depth returns the approximate number of claims currently in flight —
// visible (waiting) plus not-visible (claimed, not yet acked) — mirroring
// taskpool.Queue.Depth's exact semantics and approximate-is-fine rationale
// (SQS attributes are eventually consistent; this is a reporting figure for
// `calque pool status`/`list`, not a correctness gate).
func (q *SQSQueue) Depth(ctx context.Context) (visible, inFlight int, err error) {
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("queue depth: %w", err)
	}
	visible = atoiDefault(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	inFlight = atoiDefault(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return visible, inFlight, nil
}

// DeleteQueue removes the pool's queue entirely — the destructive half of
// `calque pool delete`'s full teardown (calque#130), mirroring
// taskpool.Queue.Delete's shape exactly. No SQS queue expires on its own
// (MessageRetentionPeriod drops messages, not the queue), so this explicit
// call is a queue's only end of life today; calque#228's teardown is the
// backstop, finding a leftover queue by its calque#227 tags.
func (q *SQSQueue) DeleteQueue(ctx context.Context) error {
	_, err := q.client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(q.url)})
	if err != nil {
		return fmt.Errorf("delete pool queue: %w", err)
	}
	return nil
}

// DeletePoolQueueIfExists deletes a pool's queue by model name, tolerating the
// queue already being gone (a second `calque pool delete` on an already-torn-
// down pool must not fail). Deliberately does a single GetQueueUrl lookup
// rather than reusing OpenPoolQueue's up-to-10-attempt eventual-consistency
// retry loop: that retry budget exists to wait out a FRESH CreatePoolQueue's
// propagation lag, which has no bearing on a delete — a queue that isn't
// visible on the first lookup here is either already deleted or never
// existed, and either way there is nothing further to wait for.
func DeletePoolQueueIfExists(ctx context.Context, client SQSAPI, model string) error {
	return calquefootprint.DeleteQueueIfExists(ctx, client, PoolQueueName(model))
}

// RunQueueName derives the SQS queue name for a fleet run's shard queue,
// keyed by RUN ID (not model) — the opposite scoping decision from
// PoolQueueName, and deliberately so (calque#145): a fleet run's queue does
// NOT outlive the run the way a pool's queue outlives any one run, so it's
// scoped to the run that created it (and deleted by that run's cleanup,
// DeleteRunQueueIfExists), mirroring spawn's own taskpool.QueueName(runID)
// shape exactly rather than PoolQueueName's model-scoped one. Slugged the
// same way PoolQueueName slugs a model — a runID is expected to already be
// SQS-safe (calque's own runIDs are typically date-stamped identifiers), but
// nothing upstream enforces that, so this stays defensive rather than
// trusting the caller.
func RunQueueName(runID string) string {
	return fleetQueuePrefix + slugModel(runID)
}

// fleetQueuePrefix mirrors poolQueuePrefix's role for RunQueueName — a named
// const so slugModel's length budget stays in lockstep with the prefix here
// too.
const fleetQueuePrefix = calquefootprint.FleetQueuePrefix

// runQueueRetentionSeconds is a fleet run queue's MessageRetentionPeriod:
// 12h, mirroring spawn's taskpool.CreateQueue. It bounds how long a shard's
// claim can sit unclaimed. It does NOT expire the queue: SQS retention drops
// messages, never the queue itself (calque#227 corrected a comment that said
// otherwise). A run queue whose run's cleanup failed stays until something
// deletes it; it is tagged (RunQueueTags) so calque#228's teardown can.
const runQueueRetentionSeconds = "43200"

// CreateRunQueue creates (idempotently) the shard queue for one fleet run,
// keyed by runID (calque#145) — the run-scoped sibling to CreatePoolQueue's
// model-scoped queue, with a shorter message retention
// (runQueueRetentionSeconds). Neither retention removes the queue: the run's
// own teardown (DeleteRunQueueIfExists) does, and if that is skipped the
// queue stays, tagged RunQueueTags(runID) (calque#227) so it can be found.
func CreateRunQueue(ctx context.Context, client SQSAPI, runID string, visibilityTimeout int) (*SQSQueue, error) {
	return createTaggedQueue(ctx, client, RunQueueName(runID), map[string]string{
		"VisibilityTimeout":      fmt.Sprintf("%d", visibilityTimeout),
		"MessageRetentionPeriod": runQueueRetentionSeconds,
	}, RunQueueTags(runID, time.Now()))
}

// OpenRunQueue resolves an EXISTING fleet run's shard queue by run id (the
// worker path: a fleet worker boots knowing only its run id, mirroring
// OpenPoolQueue's worker-boots-knowing-only-its-model shape). Retries
// GetQueueUrl's eventual-consistency window after a fresh CreateRunQueue,
// reusing the exact same retry loop/constants OpenPoolQueue does.
func OpenRunQueue(ctx context.Context, client SQSAPI, runID string) (*SQSQueue, error) {
	name := RunQueueName(runID)
	var lastErr error
	for attempt := 0; attempt < poolOpenMaxAttempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(poolOpenRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err == nil {
			return &SQSQueue{client: client, url: aws.ToString(out.QueueUrl)}, nil
		}
		lastErr = err
		if !isNonExistentQueue(err) {
			return nil, fmt.Errorf("resolve run queue %s: %w", name, err)
		}
	}
	return nil, fmt.Errorf("resolve run queue %s: not visible after %d attempts: %w", name, poolOpenMaxAttempts, lastErr)
}

// DeleteRunQueueIfExists deletes a fleet run's shard queue by run id,
// tolerating the queue already being gone — mirrors
// DeletePoolQueueIfExists's exact idempotent-delete shape and rationale.
func DeleteRunQueueIfExists(ctx context.Context, client SQSAPI, runID string) error {
	return calquefootprint.DeleteQueueIfExists(ctx, client, RunQueueName(runID))
}

// atoiDefault parses a decimal string, returning 0 on any non-digit —
// mirrors taskpool's own helper of the same name and purpose (SQS attribute
// values are always decimal strings; a malformed one is worth reporting as 0,
// not panicking a status call over).
func atoiDefault(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
