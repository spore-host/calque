package plan

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/internal/calquetags"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/ec2tags"
	"github.com/spore-host/libs/tags/tagtest"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// ---- calque#227: the real-run role and the NFS security group are tagged ------

// TestRealRunInstanceProfile_TaggedThroughSpawn drives spawn v0.131.0's real
// CreateOrGetInstanceProfile: its CreateRole and CreateInstanceProfile must
// carry managed-by=calque (tagtest), scoped to the bucket.
func TestRealRunInstanceProfile_TaggedThroughSpawn(t *testing.T) {
	f := awstest.NewIAM()
	cfg := awstest.Config(awstest.Services{"IAM": f.Handle, "STS": awstest.STS("111122223333")})
	d := tagtest.New(t, tags.ToolCalque)
	d.Install(&cfg)

	profile, err := RealRunInstanceProfile(context.Background(), spawnaws.NewClientFromConfig(cfg), "us-east-1", "my-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if profile != roleNameForBucket("my-bucket") {
		t.Errorf("profile = %q, want %q", profile, roleNameForBucket("my-bucket"))
	}
	var tagged []string
	for _, c := range d.Calls() {
		if c.Tagged {
			tagged = append(tagged, c.Operation)
		}
	}
	if strings.Join(tagged, ",") != "CreateRole,CreateInstanceProfile" {
		t.Errorf("tagged creates = %v, want CreateRole and CreateInstanceProfile", tagged)
	}
	for _, got := range []map[string]string{f.RoleTags(profile), f.ProfileTags(profile)} {
		if !tags.IsManagedBy(got, tags.ToolCalque) || got[tags.Scope] != "bucket:my-bucket" || got["spawn:managed"] != "true" {
			t.Errorf("tags = %v, want calque's set (scope bucket:my-bucket) beside spawn's", got)
		}
	}
}

// TestRealRunInstanceProfile_TagsAnOlderRole: spawn reuses an existing role
// without tagging it; calque adds its set.
func TestRealRunInstanceProfile_TagsAnOlderRole(t *testing.T) {
	f := awstest.NewIAM()
	name := roleNameForBucket("my-bucket")
	f.SeedRole(name, map[string]string{"spawn:managed": "true"})
	cfg := awstest.Config(awstest.Services{"IAM": f.Handle, "STS": awstest.STS("111122223333")})
	if _, err := RealRunInstanceProfile(context.Background(), spawnaws.NewClientFromConfig(cfg), "us-east-1", "my-bucket"); err != nil {
		t.Fatal(err)
	}
	if got := f.RoleTags(name); !tags.IsManagedBy(got, tags.ToolCalque) {
		t.Errorf("role tags = %v, want managed-by=calque added", got)
	}
}

func TestRealRunPolicy_GrantsPutObjectTaggingOnTheStagingBucketOnly(t *testing.T) {
	doc := RealRunPolicy("111122223333", "us-east-1", "stage", []string{"mount"})
	if !strings.Contains(doc, `"s3:GetObject","s3:PutObject","s3:PutObjectTagging"],"Resource":["arn:aws:s3:::stage/*"]`) {
		t.Errorf("staging bucket statement lacks s3:PutObjectTagging: %s", doc)
	}
	if strings.Count(doc, "s3:PutObjectTagging") != 1 {
		t.Errorf("s3:PutObjectTagging granted beyond the staging bucket: %s", doc)
	}
}

// ec2SG answers the EC2 calls EnsureNFSSecurityGroup makes. existing, when
// set, is the group DescribeSecurityGroups finds.
type ec2SG struct {
	mu       sync.Mutex
	existing *ec2types.SecurityGroup
	created  *ec2.CreateSecurityGroupInput
	tagged   []*ec2.CreateTagsInput
}

func (e *ec2SG) handle(op string, in any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch v := in.(type) {
	case *ec2.DescribeSecurityGroupsInput:
		if e.existing == nil {
			return &ec2.DescribeSecurityGroupsOutput{}, nil
		}
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{*e.existing}}, nil
	case *ec2.CreateSecurityGroupInput:
		e.created = v
		return &ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-new")}, nil
	case *ec2.AuthorizeSecurityGroupIngressInput:
		return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
	case *ec2.CreateTagsInput:
		e.tagged = append(e.tagged, v)
		return &ec2.CreateTagsOutput{}, nil
	}
	return nil, awstest.Unhandled("EC2", op)
}

// TestEnsureNFSSecurityGroup_TaggedAtCreate: CreateSecurityGroup carries
// managed-by=calque in its TagSpecifications (tagtest), scoped to the VPC.
func TestEnsureNFSSecurityGroup_TaggedAtCreate(t *testing.T) {
	e := &ec2SG{}
	cfg := awstest.Config(awstest.Services{"EC2": e.handle})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	id, err := EnsureNFSSecurityGroup(context.Background(), ec2.NewFromConfig(cfg), "vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "sg-new" {
		t.Errorf("id = %q, want sg-new", id)
	}
	if e.created == nil || len(e.created.TagSpecifications) != 1 || e.created.TagSpecifications[0].ResourceType != ec2types.ResourceTypeSecurityGroup {
		t.Fatalf("CreateSecurityGroup TagSpecifications = %+v, want one for the security group", e.created)
	}
	got := ec2tags.ToMap(e.created.TagSpecifications[0].Tags)
	if got[tags.Scope] != "vpc:vpc-1" || got[calquetags.Managed] != "true" {
		t.Errorf("security group tags = %v, want scope vpc:vpc-1 and calque:managed", got)
	}
	if len(e.tagged) != 0 {
		t.Errorf("CreateTags called on a group tagged at create")
	}
}

// TestEnsureNFSSecurityGroup_TagsAnOlderGroup: a calque-nfs-sg an older
// calque created untagged is found, reused, and tagged with CreateTags.
func TestEnsureNFSSecurityGroup_TagsAnOlderGroup(t *testing.T) {
	e := &ec2SG{existing: &ec2types.SecurityGroup{GroupId: aws.String("sg-old"), GroupName: aws.String(nfsSecurityGroupName)}}
	cfg := awstest.Config(awstest.Services{"EC2": e.handle})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	id, err := EnsureNFSSecurityGroup(context.Background(), ec2.NewFromConfig(cfg), "vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "sg-old" || e.created != nil {
		t.Fatalf("id = %q, created = %v; want the existing group reused", id, e.created)
	}
	if len(e.tagged) != 1 || e.tagged[0].Resources[0] != "sg-old" || !tags.IsManagedBy(ec2tags.ToMap(e.tagged[0].Tags), tags.ToolCalque) {
		t.Errorf("CreateTags = %+v, want one tagging sg-old managed-by=calque", e.tagged)
	}

	// Already tagged: nothing more to write.
	e.existing.Tags = ec2tags.FromMap(NFSSecurityGroupTags("vpc-1", t0Plan))
	e.tagged = nil
	if _, err := EnsureNFSSecurityGroup(context.Background(), ec2.NewFromConfig(cfg), "vpc-1"); err != nil {
		t.Fatal(err)
	}
	if len(e.tagged) != 0 {
		t.Errorf("CreateTags on an already-tagged group: %+v", e.tagged)
	}
}

// TestEnsureNFSSecurityGroup_TagFailureIsAWarning: the group still works.
func TestEnsureNFSSecurityGroup_TagFailureIsAWarning(t *testing.T) {
	var warn bytes.Buffer
	prev := calquetags.Warn
	calquetags.Warn = &warn
	t.Cleanup(func() { calquetags.Warn = prev })
	e := &ec2SG{existing: &ec2types.SecurityGroup{GroupId: aws.String("sg-old")}}
	cfg := awstest.Config(awstest.Services{"EC2": func(op string, in any) (any, error) {
		if op == "CreateTags" {
			return nil, &awstest.APIError{Code: "UnauthorizedOperation"}
		}
		return e.handle(op, in)
	}})
	if id, err := EnsureNFSSecurityGroup(context.Background(), ec2.NewFromConfig(cfg), "vpc-1"); err != nil || id != "sg-old" {
		t.Fatalf("EnsureNFSSecurityGroup = %q, %v; want sg-old, nil", id, err)
	}
	if !strings.Contains(warn.String(), "ec2:CreateTags") {
		t.Errorf("warning = %q, want it to name ec2:CreateTags", warn.String())
	}
}

var t0Plan = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
