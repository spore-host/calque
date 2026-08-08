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

// poolSummary is the completion record a pool claim writes — deliberately
// smaller than cmd/warmd's full Summary (no occupancy/enter-seconds
// bookkeeping here; those belong to the AMORTIZED cost-attribution work,
// calque#102, tracked separately so this issue doesn't grow to cover it).
type poolSummary struct {
	Failed []int `json:"failed"`
}

// WriteSummary implements ResultWriter.
func (r *S3Results) WriteSummary(ctx context.Context, man calexec.Manifest, failed []int) error {
	body, err := json.Marshal(poolSummary{Failed: failed})
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
