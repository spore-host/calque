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
	client taskpool.SQSAPI
	url    string
}

var _ Queue = (*SQSQueue)(nil)

// CreatePoolQueue creates (idempotently) the queue for a named model pool.
// visibilityTimeout must exceed the longest expected single-claim drain time,
// or a slow claim gets redelivered and double-run while still in flight —
// same sizing rule as taskpool.CreateQueue.
func CreatePoolQueue(ctx context.Context, client taskpool.SQSAPI, model string, visibilityTimeout int) (*SQSQueue, error) {
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
func OpenPoolQueue(ctx context.Context, client taskpool.SQSAPI, model string) (*SQSQueue, error) {
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
