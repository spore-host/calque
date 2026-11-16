package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/ec2tags"
	"github.com/spore-host/libs/tags/tagtest"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// TestBakedAMITags_ReachImageAndSnapshots (calque#227) drives spawn v0.131.0's
// real CreateAMI with the tags `ami bake` passes it. tagtest checks every
// TagSpecification of the CreateImage call, so the image AND its snapshots
// must both carry managed-by=calque; the calque:baked-* pair `ami list` reads
// must survive too.
func TestBakedAMITags_ReachImageAndSnapshots(t *testing.T) {
	var got *ec2.CreateImageInput
	cfg := awstest.Config(awstest.Services{"EC2": func(op string, in any) (any, error) {
		v, ok := in.(*ec2.CreateImageInput)
		if !ok {
			return nil, awstest.Unhandled("EC2", op)
		}
		got = v
		return &ec2.CreateImageOutput{ImageId: aws.String("ami-1")}, nil
	}})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	_, err := spawnaws.NewClientFromConfig(cfg).CreateAMI(context.Background(), "us-east-1", spawnaws.CreateAMIInput{
		InstanceID: "i-1", Name: "baked", Description: "d",
		Tags: bakedAMITags("vllm/vllm-openai:latest", "bake-1", time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[ec2types.ResourceType]bool{}
	for _, spec := range got.TagSpecifications {
		kinds[spec.ResourceType] = true
		m := ec2tags.ToMap(spec.Tags)
		if m[tags.Scope] != "run:bake-1" || m[bakedImageTagKey] != "vllm/vllm-openai:latest" || m[bakedAtTagKey] == "" {
			t.Errorf("%s tags = %v, want scope run:bake-1 and the calque:baked-* pair", spec.ResourceType, m)
		}
	}
	if !kinds[ec2types.ResourceTypeImage] || !kinds[ec2types.ResourceTypeSnapshot] {
		t.Errorf("TagSpecifications cover %v, want image and snapshot", kinds)
	}
}
