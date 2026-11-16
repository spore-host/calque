package calquefootprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithy "github.com/aws/smithy-go"

	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/ec2tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/s3tags"
)

// IAMAPI is the slice of the IAM SDK the footprint and teardown use. A real
// *iam.Client satisfies it.
type IAMAPI interface {
	ListRoles(ctx context.Context, in *iam.ListRolesInput, opts ...func(*iam.Options)) (*iam.ListRolesOutput, error)
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	GetRolePolicy(ctx context.Context, in *iam.GetRolePolicyInput, opts ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	ListInstanceProfiles(ctx context.Context, in *iam.ListInstanceProfilesInput, opts ...func(*iam.Options)) (*iam.ListInstanceProfilesOutput, error)
	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	ListInstanceProfilesForRole(ctx context.Context, in *iam.ListInstanceProfilesForRoleInput, opts ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)

	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, opts ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, opts ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	RemoveRoleFromInstanceProfile(ctx context.Context, in *iam.RemoveRoleFromInstanceProfileInput, opts ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(ctx context.Context, in *iam.DeleteInstanceProfileInput, opts ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, opts ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

// SQSAPI is the slice of the SQS SDK the footprint and teardown use.
type SQSAPI interface {
	QueueDeleter
	ListQueues(ctx context.Context, in *sqs.ListQueuesInput, opts ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, opts ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	ListQueueTags(ctx context.Context, in *sqs.ListQueueTagsInput, opts ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
}

// QueueDeleter is the two SQS calls DeleteQueueIfExists makes.
type QueueDeleter interface {
	GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, opts ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	DeleteQueue(ctx context.Context, in *sqs.DeleteQueueInput, opts ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
}

// EC2API is the slice of the EC2 SDK the footprint and teardown use.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, opts ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)

	DescribeSecurityGroupRules(ctx context.Context, in *ec2.DescribeSecurityGroupRulesInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, opts ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, in *ec2.RevokeSecurityGroupEgressInput, opts ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error)
	DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, opts ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// S3API is the slice of the S3 SDK the footprint and teardown use.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, opts ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Clients supplies the AWS clients. SQS and EC2 are per region; S3 is per
// bucket, and also says the bucket's region. A nil S3 is fine when no
// buckets are given. ClientsFromConfig builds all of them from an aws.Config.
type Clients struct {
	IAM IAMAPI
	SQS func(region string) (SQSAPI, error)
	EC2 func(region string) (EC2API, error)
	S3  func(ctx context.Context, bucket string) (S3API, string, error)
}

// Options says what to collect.
type Options struct {
	// Account is the account ID (required).
	Account string
	// Partition is the ARN partition; "aws" if empty.
	Partition string
	// Regions are listed for queues, security groups, AMIs and instances.
	Regions []string
	// AllRegions says Regions is every region enabled in the account (see
	// Snapshot.AllRegions).
	AllRegions bool
	// Buckets are the S3 buckets to look in. calque writes into buckets the
	// caller names and keeps no record of them, so it cannot find them
	// itself: only these buckets, and only under S3Prefixes, are listed, and
	// only objects tagged spore-host:managed-by=calque are reported.
	Buckets []string
	// MaxObjects caps the objects listed per bucket (one GetObjectTagging
	// each); 0 means 10000. Past it, a warning says the bucket was cut short.
	MaxObjects int
}

// Collect reads calque's resources in an account into a Snapshot. A listing
// that fails is recorded in Snapshot.Incomplete and as a warning, not
// returned: the rest is still worth reporting, and Classify treats what it
// could not see as active. The error is only for bad Options.
func Collect(ctx context.Context, c Clients, o Options) (*Snapshot, error) {
	if o.Account == "" {
		return nil, errors.New("calquefootprint: Options.Account is required")
	}
	if c.IAM == nil || c.SQS == nil || c.EC2 == nil {
		return nil, errors.New("calquefootprint: Clients.IAM, SQS and EC2 are required")
	}
	if len(o.Buckets) > 0 && c.S3 == nil {
		return nil, errors.New("calquefootprint: Clients.S3 is required when Options.Buckets is set")
	}
	s := &Snapshot{Account: o.Account, Partition: o.Partition, Regions: append([]string(nil), o.Regions...), AllRegions: o.AllRegions, Incomplete: map[string]bool{}}
	fail := func(key string, err error) {
		s.Incomplete[key] = true
		s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %v", key, err))
	}
	if err := collectIAM(ctx, c.IAM, s); err != nil {
		fail("iam", err)
	}
	for _, region := range o.Regions {
		collectRegion(ctx, c, region, s, fail)
	}
	for _, b := range o.Buckets {
		if err := collectBucket(ctx, c, b, o.MaxObjects, s); err != nil {
			fail("s3:"+b, err)
		}
	}
	return s, nil
}

// Inventory is Collect then Classify, as of now, with the default grace.
func Inventory(ctx context.Context, c Clients, o Options, now time.Time) (*Footprint, error) {
	s, err := Collect(ctx, c, o)
	if err != nil {
		return nil, err
	}
	return Classify(s, now, Grace), nil
}

func isCalqueRoleName(name string) bool { return matchesName(TypeRole, name) }

func collectIAM(ctx context.Context, api IAMAPI, s *Snapshot) error {
	var marker *string
	for {
		out, err := api.ListRoles(ctx, &iam.ListRolesInput{Marker: marker})
		if err != nil {
			return fmt.Errorf("list roles: %w", err)
		}
		for _, r := range out.Roles {
			name := aws.ToString(r.RoleName)
			if !isCalqueRoleName(name) {
				continue
			}
			role, err := readRole(ctx, api, name)
			if err != nil {
				return err
			}
			s.Roles = append(s.Roles, role)
		}
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		marker = out.Marker
	}
	marker = nil
	for {
		out, err := api.ListInstanceProfiles(ctx, &iam.ListInstanceProfilesInput{Marker: marker})
		if err != nil {
			return fmt.Errorf("list instance profiles: %w", err)
		}
		for _, p := range out.InstanceProfiles {
			name := aws.ToString(p.InstanceProfileName)
			if !isCalqueRoleName(name) {
				continue
			}
			prof := Profile{Name: name, ARN: aws.ToString(p.Arn), Created: aws.ToTime(p.CreateDate)}
			for _, r := range p.Roles {
				prof.Roles = append(prof.Roles, aws.ToString(r.RoleName))
			}
			// ListInstanceProfiles returns no tags.
			got, err := api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
			if err != nil {
				return fmt.Errorf("get instance profile %s: %w", name, err)
			}
			if got.InstanceProfile != nil {
				prof.Tags = iamtags.ToMap(got.InstanceProfile.Tags)
			}
			s.Profiles = append(s.Profiles, prof)
		}
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		marker = out.Marker
	}
	return nil
}

// readRole reads a role's tags, inline policies (with each grant's queue)
// and attached managed policies.
func readRole(ctx context.Context, api IAMAPI, name string) (Role, error) {
	got, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		return Role{}, fmt.Errorf("get role %s: %w", name, err)
	}
	role := Role{Name: name}
	if got.Role != nil {
		role.ARN = aws.ToString(got.Role.Arn)
		role.Tags = iamtags.ToMap(got.Role.Tags)
		role.Created = aws.ToTime(got.Role.CreateDate)
	}
	inline, err := listInlinePolicies(ctx, api, name)
	if err != nil {
		return Role{}, err
	}
	role.InlinePolicies = inline
	for _, p := range inline {
		if !isWorkerRole(name) || !matchesName(TypeRolePolicy, p) {
			continue
		}
		g := Grant{Policy: p}
		if doc, err := api.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(p)}); err == nil {
			if qs := sqsResources(aws.ToString(doc.PolicyDocument)); len(qs) == 1 {
				g.QueueARN = qs[0]
			}
		}
		role.Grants = append(role.Grants, g)
	}
	role.ManagedPolicies, err = listAttached(ctx, api, name)
	if err != nil {
		return Role{}, err
	}
	return role, nil
}

func listInlinePolicies(ctx context.Context, api IAMAPI, role string) ([]string, error) {
	var names []string
	var marker *string
	for {
		out, err := api.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(role), Marker: marker})
		if err != nil {
			return nil, fmt.Errorf("list role %s inline policies: %w", role, err)
		}
		names = append(names, out.PolicyNames...)
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		marker = out.Marker
	}
	sort.Strings(names)
	return names, nil
}

func listAttached(ctx context.Context, api IAMAPI, role string) ([]string, error) {
	var arns []string
	var marker *string
	for {
		out, err := api.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role), Marker: marker})
		if err != nil {
			return nil, fmt.Errorf("list role %s managed policies: %w", role, err)
		}
		for _, p := range out.AttachedPolicies {
			arns = append(arns, aws.ToString(p.PolicyArn))
		}
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		marker = out.Marker
	}
	sort.Strings(arns)
	return arns, nil
}

// sqsResources returns the SQS ARNs a policy document grants, sorted and
// de-duplicated; nil if the document cannot be parsed. GetRolePolicy returns
// the document URL-encoded.
func sqsResources(doc string) []string {
	if decoded, err := url.PathUnescape(doc); err == nil {
		doc = decoded
	}
	var p struct {
		Statement []struct {
			Resource json.RawMessage `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, st := range p.Statement {
		var many []string
		if err := json.Unmarshal(st.Resource, &many); err != nil {
			var one string
			if err := json.Unmarshal(st.Resource, &one); err != nil {
				continue
			}
			many = []string{one}
		}
		for _, r := range many {
			if strings.HasPrefix(r, "arn:") && strings.Contains(r, ":sqs:") {
				seen[r] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// liveStates are the instance states that count as live.
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

func collectRegion(ctx context.Context, c Clients, region string, s *Snapshot, fail func(string, error)) {
	if sq, err := c.SQS(region); err != nil {
		fail("sqs:"+region, err)
	} else if err := collectQueues(ctx, sq, region, s); err != nil {
		fail("sqs:"+region, err)
	}
	e, err := c.EC2(region)
	if err != nil {
		fail("ec2:"+region, err)
		fail("eni:"+region, err)
		return
	}
	if err := collectInstances(ctx, e, region, s); err != nil {
		fail("ec2:"+region, err)
	}
	if err := collectSecurityGroups(ctx, e, region, s); err != nil {
		fail("eni:"+region, err)
	}
	if err := collectImages(ctx, e, region, s); err != nil {
		fail("ami:"+region, err)
	}
}

func collectQueues(ctx context.Context, api SQSAPI, region string, s *Snapshot) error {
	for _, prefix := range []string{PoolQueuePrefix, FleetQueuePrefix} {
		var token *string
		for {
			out, err := api.ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String(prefix), NextToken: token, MaxResults: aws.Int32(1000)})
			if err != nil {
				return fmt.Errorf("list queues %s*: %w", prefix, err)
			}
			for _, u := range out.QueueUrls {
				q, err := readQueue(ctx, api, region, u)
				if IsQueueNotFound(err) {
					continue // deleted since the listing
				}
				if err != nil {
					return err
				}
				s.Queues = append(s.Queues, q)
			}
			if out.NextToken == nil {
				break
			}
			token = out.NextToken
		}
	}
	return nil
}

func readQueue(ctx context.Context, api SQSAPI, region, queueURL string) (Queue, error) {
	q := Queue{Region: region, URL: queueURL, Name: arnName(queueURL)}
	attrs, err := api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameQueueArn,
			sqstypes.QueueAttributeNameCreatedTimestamp,
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return q, fmt.Errorf("queue %s attributes: %w", q.Name, err)
	}
	a := attrs.Attributes
	q.ARN = a[string(sqstypes.QueueAttributeNameQueueArn)]
	q.Visible = atoi(a[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	q.InFlight = atoi(a[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	q.Delayed = atoi(a[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed)])
	if ts := atoi(a[string(sqstypes.QueueAttributeNameCreatedTimestamp)]); ts > 0 {
		q.Created = time.Unix(int64(ts), 0).UTC()
	}
	t, err := api.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(queueURL)})
	if err != nil {
		return q, fmt.Errorf("queue %s tags: %w", q.Name, err)
	}
	q.Tags = t.Tags
	return q, nil
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func liveInstanceFilters(extra ...ec2types.Filter) []ec2types.Filter {
	return append([]ec2types.Filter{{Name: aws.String("instance-state-name"), Values: liveStates}}, extra...)
}

func collectInstances(ctx context.Context, api EC2API, region string, s *Snapshot) error {
	ins, err := describeInstances(ctx, api, region, liveInstanceFilters(ec2types.Filter{Name: aws.String("tag:" + managedTag), Values: []string{"true"}}))
	if err != nil {
		return err
	}
	s.Instances = append(s.Instances, ins...)
	return nil
}

func describeInstances(ctx context.Context, api EC2API, region string, filters []ec2types.Filter, ids ...string) ([]Instance, error) {
	var out []Instance
	var token *string
	for {
		in := &ec2.DescribeInstancesInput{Filters: filters, NextToken: token}
		if len(ids) > 0 {
			in.InstanceIds = ids
		}
		got, err := api.DescribeInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range got.Reservations {
			for _, i := range r.Instances {
				inst := Instance{Region: region, ID: aws.ToString(i.InstanceId), Tags: ec2tags.ToMap(i.Tags)}
				if i.State != nil {
					inst.State = string(i.State.Name)
				}
				if i.IamInstanceProfile != nil {
					inst.ProfileARN = aws.ToString(i.IamInstanceProfile.Arn)
				}
				out = append(out, inst)
			}
		}
		if got.NextToken == nil {
			return out, nil
		}
		token = got.NextToken
	}
}

func collectSecurityGroups(ctx context.Context, api EC2API, region string, s *Snapshot) error {
	out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-name"), Values: []string{NFSSecurityGroupName}}},
	})
	if err != nil {
		return fmt.Errorf("describe security groups: %w", err)
	}
	for _, g := range out.SecurityGroups {
		sg := SecurityGroup{Region: region, ID: aws.ToString(g.GroupId), Name: aws.ToString(g.GroupName), VPC: aws.ToString(g.VpcId), Tags: ec2tags.ToMap(g.Tags)}
		sg.ENIs, err = groupENIs(ctx, api, sg.ID)
		if err != nil {
			return err
		}
		s.SecurityGroups = append(s.SecurityGroups, sg)
	}
	return nil
}

func groupENIs(ctx context.Context, api EC2API, groupID string) ([]string, error) {
	var ids []string
	var token *string
	for {
		out, err := api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			Filters:   []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{groupID}}},
			NextToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("describe network interfaces of %s: %w", groupID, err)
		}
		for _, n := range out.NetworkInterfaces {
			ids = append(ids, aws.ToString(n.NetworkInterfaceId))
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	sort.Strings(ids)
	return ids, nil
}

func collectImages(ctx context.Context, api EC2API, region string, s *Snapshot) error {
	baked := []ec2types.Filter{{Name: aws.String("tag-key"), Values: []string{BakedImageTag}}}
	imgs, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: baked})
	if err != nil {
		return fmt.Errorf("describe images: %w", err)
	}
	for _, im := range imgs.Images {
		img := Image{Region: region, ID: aws.ToString(im.ImageId), Name: aws.ToString(im.Name), State: string(im.State), Tags: ec2tags.ToMap(im.Tags)}
		for _, bd := range im.BlockDeviceMappings {
			if bd.Ebs != nil && bd.Ebs.SnapshotId != nil {
				img.Snapshots = append(img.Snapshots, *bd.Ebs.SnapshotId)
			}
		}
		s.Images = append(s.Images, img)
	}
	var token *string
	for {
		snaps, err := api.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{OwnerIds: []string{"self"}, Filters: baked, NextToken: token})
		if err != nil {
			return fmt.Errorf("describe snapshots: %w", err)
		}
		for _, sn := range snaps.Snapshots {
			s.Snapshots = append(s.Snapshots, EBSSnapshot{Region: region, ID: aws.ToString(sn.SnapshotId), Tags: ec2tags.ToMap(sn.Tags)})
		}
		if snaps.NextToken == nil {
			return nil
		}
		token = snaps.NextToken
	}
}

// objectWorkers bounds the concurrent GetObjectTagging calls per bucket.
const objectWorkers = 8

func collectBucket(ctx context.Context, c Clients, bucket string, maxObjects int, s *Snapshot) error {
	if maxObjects <= 0 {
		maxObjects = 10000
	}
	api, region, err := c.S3(ctx, bucket)
	if err != nil {
		return err
	}
	var keys []s3Key
	truncated := false
	for _, prefix := range S3Prefixes {
		var token *string
		for !truncated {
			out, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), ContinuationToken: token})
			if err != nil {
				return fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
			}
			for _, o := range out.Contents {
				if len(keys) == maxObjects {
					truncated = true
					break
				}
				keys = append(keys, s3Key{key: aws.ToString(o.Key), modified: aws.ToTime(o.LastModified), size: aws.ToInt64(o.Size)})
			}
			if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
				break
			}
			token = out.NextContinuationToken
		}
	}
	if truncated {
		s.Warnings = append(s.Warnings, fmt.Sprintf("s3://%s: stopped after %d objects; the rest are not reported", bucket, maxObjects))
	}
	objs := make([]*Object, len(keys))
	errs := make([]error, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, objectWorkers)
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(k.key)})
			if err != nil {
				if !isS3NotFound(err) {
					errs[i] = fmt.Errorf("tags of s3://%s/%s: %w", bucket, k.key, err)
				}
				return
			}
			m := s3tags.ToMap(t.TagSet)
			if tags.IsManagedBy(m, Tool) {
				objs[i] = &Object{Bucket: bucket, Region: region, Key: k.key, Tags: m, LastModified: k.modified, Size: k.size}
			}
		}()
	}
	wg.Wait()
	untagged := 0
	for i, o := range objs {
		switch {
		case o != nil:
			s.Objects = append(s.Objects, *o)
		case errs[i] == nil:
			untagged++
		}
	}
	if untagged > 0 {
		s.Warnings = append(s.Warnings, fmt.Sprintf("s3://%s: %d object(s) under %s not tagged %s=%s, not reported",
			bucket, untagged, strings.Join(S3Prefixes, ", "), tags.ManagedBy, Tool))
	}
	return errors.Join(errs...)
}

type s3Key struct {
	key      string
	modified time.Time
	size     int64
}

// IsQueueNotFound reports whether err says an SQS queue does not exist.
func IsQueueNotFound(err error) bool {
	if err == nil {
		return false
	}
	var qde *sqstypes.QueueDoesNotExist
	if errors.As(err, &qde) {
		return true
	}
	code := errorCode(err)
	return code == "AWS.SimpleQueueService.NonExistentQueue" || code == "QueueDoesNotExist"
}

func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func isS3NotFound(err error) bool {
	switch errorCode(err) {
	case "NoSuchKey", "NotFound", "NoSuchBucket":
		return true
	}
	return false
}
