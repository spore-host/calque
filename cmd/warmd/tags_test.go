package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/internal/calquetags"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"
)

// TestPutJSON_TagsTheSummary (calque#227): the run summary warmd writes from
// the instance carries managed-by=calque and the manifest's run id.
func TestPutJSON_TagsTheSummary(t *testing.T) {
	var tagging string
	cfg := awstest.Config(awstest.Services{"S3": func(op string, in any) (any, error) {
		v, ok := in.(*s3.PutObjectInput)
		if !ok {
			return nil, awstest.Unhandled("S3", op)
		}
		tagging = aws.ToString(v.Tagging)
		return &s3.PutObjectOutput{}, nil
	}})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	if err := putJSON(context.Background(), s3.NewFromConfig(cfg), "b", "runs/r5/summary.json", calquetags.ObjectTagging("r5"), map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tagging, "calque%3Arun-id=r5") {
		t.Errorf("Tagging = %q, want calque:run-id=r5", tagging)
	}
}
