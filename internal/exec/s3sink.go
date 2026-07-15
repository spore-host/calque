// Package exec runs the plan on real hardware (spec §5): it builds+pushes the
// worker image, drives acquisition, and collects results from S3 ordered by input
// index. The S3 sink here is what the on-instance warmd writes to; the collector
// reads it back in order.
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	warm "github.com/spore-host/calque/worker/warm-runner"
)

// S3Sink writes each warm-runner result to S3 keyed by input index, so results
// can be collected in order regardless of completion order (spec §6: "keyed by
// input index for ordered collection"). One object per item keeps memory flat at
// 100k scale — no buffering the whole result set.
type S3Sink struct {
	Client *s3.Client
	Bucket string
	Prefix string // e.g. "runs/<runID>/results"

	// secs accumulates the per-item wall-clock series for the tach hook (§8). One
	// float per item is cheap even at 100k (~800KB); the results themselves stream
	// to S3 and are never buffered.
	secs []float64
}

// Seconds returns the per-item wall-clock series collected so far (for the run
// summary the on-instance warmd writes back).
func (s *S3Sink) Seconds() []float64 { return s.secs }

// key is the S3 object key for a result index, zero-padded so lexical listing is
// also numeric-ordered up to 10 digits (100k fits comfortably).
func (s *S3Sink) key(index int) string {
	return fmt.Sprintf("%s/%010d.json", s.Prefix, index)
}

// Put writes one result. Implements warm.Sink.
func (s *S3Sink) Put(ctx context.Context, r warm.Result) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.key(r.Index)),
		Body:   bytes.NewReader(body),
	})
	if err == nil {
		s.secs = append(s.secs, r.Seconds)
	}
	return err
}

var _ warm.Sink = (*S3Sink)(nil)

// Collect reads all results under the prefix and returns them ordered by index —
// the ordered collection step (§3). It also reports which indices are MISSING
// (partial failure — "3 of 10k items die", §10), so the caller can leak them.
func Collect(ctx context.Context, client *s3.Client, bucket, prefix string, expected int) (results []warm.Result, missing []int, err error) {
	got := map[int]warm.Result{}
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix + "/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("list results: %w", err)
		}
		for _, obj := range out.Contents {
			r, err := getResult(ctx, client, bucket, *obj.Key)
			if err != nil {
				return nil, nil, err
			}
			got[r.Index] = r
		}
		if out.IsTruncated != nil && *out.IsTruncated {
			token = out.NextContinuationToken
			continue
		}
		break
	}

	results = make([]warm.Result, 0, len(got))
	for _, r := range got {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })

	for i := 0; i < expected; i++ {
		if _, ok := got[i]; !ok {
			missing = append(missing, i)
		}
	}
	return results, missing, nil
}

func getResult(ctx context.Context, client *s3.Client, bucket, key string) (warm.Result, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return warm.Result{}, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	var r warm.Result
	if err := json.NewDecoder(out.Body).Decode(&r); err != nil {
		return warm.Result{}, fmt.Errorf("decode %s: %w", key, err)
	}
	return r, nil
}
