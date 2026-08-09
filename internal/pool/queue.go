package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithy "github.com/aws/smithy-go"

	"github.com/spore-host/spawn/pkg/taskpool"
)

// SQSAPI is the slice of the SQS SDK pool.SQSQueue needs: taskpool.SQSAPI's
// own Claim/Ack-supporting methods, plus ChangeMessageVisibility for
// calque#131's heartbeat extension — which taskpool.SQSAPI does not declare
// (spawn's fungible, short-lived CPU tasks have no heartbeat need). A real
// *sqs.Client satisfies both this and taskpool.SQSAPI unchanged.
type SQSAPI interface {
	taskpool.SQSAPI
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
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
// SQS defaults (no forced retention override), rather than importing the
// run-scoped type verbatim.
type SQSQueue struct {
	client SQSAPI
	url    string
}

var _ Queue = (*SQSQueue)(nil)

// CreatePoolQueue creates (idempotently) the queue for a named model pool.
// visibilityTimeout must exceed the longest expected single-claim drain time,
// or a slow claim gets redelivered and double-run while still in flight —
// same sizing rule as taskpool.CreateQueue.
func CreatePoolQueue(ctx context.Context, client SQSAPI, model string, visibilityTimeout int) (*SQSQueue, error) {
	name := PoolQueueName(model)
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"VisibilityTimeout": fmt.Sprintf("%d", visibilityTimeout),
			// No MessageRetentionPeriod override: SQS's own default (4 days) is
			// fine here since, unlike a run-scoped queue, nothing else expires
			// this queue on a fixed schedule — its lifetime is "as long as the
			// pool exists," ended by an explicit `calque pool delete`, not time.
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create pool queue %s: %w", name, err)
	}
	return &SQSQueue{client: client, url: aws.ToString(out.QueueUrl)}, nil
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

func isNonExistentQueue(err error) bool {
	var qde *types.QueueDoesNotExist
	if errors.As(err, &qde) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "AWS.SimpleQueueService.NonExistentQueue" || code == "QueueDoesNotExist"
	}
	return false
}

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
// taskpool.Queue.Delete's shape exactly. Unlike a run-scoped taskpool queue
// (which also expires on its own via MessageRetentionPeriod), a pool queue's
// only end-of-life path is this explicit call — see CreatePoolQueue's own
// comment ("ended by an explicit `calque pool delete`, not time").
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
	name := PoolQueueName(model)
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		if isNonExistentQueue(err) {
			return nil // already gone — deletion is idempotent
		}
		return fmt.Errorf("resolve pool queue %s for deletion: %w", name, err)
	}
	q := &SQSQueue{client: client, url: aws.ToString(out.QueueUrl)}
	return q.DeleteQueue(ctx)
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
