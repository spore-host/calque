package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	calexec "github.com/spore-host/calque/internal/exec"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// S3Manifests fetches a claim's manifest JSON from S3 by its s3:// URI.
// Implements ManifestFetcher.
type S3Manifests struct {
	Client *s3.Client
}

// Fetch implements ManifestFetcher.
func (m *S3Manifests) Fetch(ctx context.Context, manifestURI string) ([]byte, error) {
	bucket, key, err := parseS3URI(manifestURI)
	if err != nil {
		return nil, err
	}
	out, err := m.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", manifestURI, err)
	}
	defer out.Body.Close()
	buf, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestURI, err)
	}
	return buf, nil
}

// S3Results is the production ResultWriter: results land via calexec.S3Sink
// (reused unmodified — the same index-keyed sink a dedicated per-run
// acquisition uses), and the completion summary is a JSON PutObject at the
// manifest's own SummaryKey, so a submitter polling via
// calexec.WaitForSummary (also reused unmodified) sees the claim complete
// exactly as it would a dedicated run's.
type S3Results struct {
	Client *s3.Client
}

// Sink implements ResultWriter.
func (r *S3Results) Sink(man calexec.Manifest) warm.Sink {
	return &calexec.S3Sink{Client: r.Client, Bucket: man.Bucket, Prefix: man.ResultPrefix}
}

// Summary is the completion record a pool claim writes — deliberately
// smaller than cmd/warmd's full Summary (no occupancy bookkeeping here). It
// carries just enough for a submitter (calque#103) to feed cost.Measured
// honestly (calque#102): WarmHit tells the submitter whether THIS claim's
// AcquireSeconds/EnterSeconds should be reported as near-zero (a pool hit)
// or the pool's own dedicated first-load cost (a miss); EnterSecondsPaid is
// the ACTUAL @enter cost this specific claim caused (0 on a clean warm hit,
// the measured load time if this claim triggered a load — including the
// rarer started-warm-but-crashed-mid-drain case). The submitter, not the
// worker, ultimately builds the cost.Model, since only the submitter knows
// the run's item count and card-asked-for. Exported (unlike cmd/warmd's own
// unexported Summary) because a submitter living in cmd/calque needs to
// decode it after calexec.WaitForSummary returns.
type Summary struct {
	Failed           []int   `json:"failed"`
	WarmHit          bool    `json:"warm_hit"`
	EnterSecondsPaid float64 `json:"enter_seconds_paid"`
}

// WriteSummary implements ResultWriter.
func (r *S3Results) WriteSummary(ctx context.Context, man calexec.Manifest, failed []int, warmHit bool, enterSecondsPaid float64) error {
	body, err := json.Marshal(Summary{Failed: failed, WarmHit: warmHit, EnterSecondsPaid: enterSecondsPaid})
	if err != nil {
		return err
	}
	_, err = r.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(man.Bucket), Key: aws.String(man.SummaryKey), Body: bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("write summary %s: %w", man.SummaryKey, err)
	}
	return nil
}

// parseS3URI splits "s3://bucket/key/with/slashes" into bucket and key.
func parseS3URI(uri string) (bucket, key string, err error) {
	const p = "s3://"
	if len(uri) <= len(p) || uri[:len(p)] != p {
		return "", "", fmt.Errorf("not an s3:// URI: %q", uri)
	}
	rest := uri[len(p):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i], rest[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("s3 URI has no key: %q", uri)
}
