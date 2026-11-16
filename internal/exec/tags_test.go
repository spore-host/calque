package exec

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/internal/calquetags"
	warm "github.com/spore-host/calque/worker/warm-runner"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"
)

// s3Puts records every PutObject: key -> (tagging, body).
type s3Puts struct {
	mu      sync.Mutex
	tagging map[string]string
	body    map[string]string
}

func (p *s3Puts) handle(op string, in any) (any, error) {
	v, ok := in.(*s3.PutObjectInput)
	if !ok {
		return nil, awstest.Unhandled("S3", op)
	}
	b, _ := io.ReadAll(v.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tagging[aws.ToString(v.Key)] = aws.ToString(v.Tagging)
	p.body[aws.ToString(v.Key)] = string(b)
	return &s3.PutObjectOutput{}, nil
}

// TestControlPlaneObjectsAreTagged (calque#227): every object the control
// plane writes for a run (artifacts, Dockerfile, manifest) and the results a
// sink writes carry managed-by=calque and calque:run-id (tagtest fails any
// that don't), and the manifest carries the run id on to warmd.
func TestControlPlaneObjectsAreTagged(t *testing.T) {
	p := &s3Puts{tagging: map[string]string{}, body: map[string]string{}}
	cfg := awstest.Config(awstest.Services{"S3": p.handle})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)
	c := s3.NewFromConfig(cfg)
	ctx := context.Background()

	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"warmd", "runner.py", "occupancy.py"} {
		path := filepath.Join(dir, n)
		if err := os.WriteFile(path, []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	l := NewLayout("bucket", "run-42")
	if err := UploadArtifacts(ctx, c, l, paths[0], paths[1], paths[2]); err != nil {
		t.Fatal(err)
	}
	if err := UploadDockerfile(ctx, c, l, "FROM scratch\n"); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(ctx, c, l, "", "", "x", "/w", []warm.Item{{Index: 0}}); err != nil {
		t.Fatal(err)
	}
	if err := (&S3Sink{Client: c, Bucket: "bucket", Prefix: l.ResultPrefix, RunID: l.RunID}).Put(ctx, warm.Result{Index: 0}); err != nil {
		t.Fatal(err)
	}

	if len(p.tagging) != 6 {
		t.Errorf("PutObject keys = %v, want 6 objects", p.tagging)
	}
	for key, tg := range p.tagging {
		q, err := url.ParseQuery(tg)
		if err != nil {
			t.Fatalf("%s: Tagging %q: %v", key, tg, err)
		}
		if q.Get(calquetags.RunID) != "run-42" || q.Get(tags.ManagedBy) != "calque" {
			t.Errorf("%s: Tagging = %q, want managed-by=calque and calque:run-id=run-42", key, tg)
		}
	}
	var man Manifest
	if err := json.Unmarshal([]byte(p.body[l.ManifestKey]), &man); err != nil {
		t.Fatal(err)
	}
	if man.RunID != "run-42" {
		t.Errorf("manifest run_id = %q, want run-42 (warmd tags its own writes with it)", man.RunID)
	}
}
