package calquefootprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/ec2tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/s3tags"
)

// ErrActive is what the Deleter's Describe returns (wrapped) for a resource
// that is in use as of the re-check: a queue with workers or messages, a role
// a live instance uses, a security group with network interfaces, an
// instance. footprint.Execute records it as a failed step, so nothing it
// depends on is deleted either.
var ErrActive = errors.New("calquefootprint: in use")

// errNotTeardown is what Delete returns for the types teardown never deletes.
var errNotTeardown = errors.New("calquefootprint: calque teardown does not delete this type")

// DeleterOptions configures NewDeleter.
type DeleterOptions struct {
	// Regions are where the re-check looks for instances using a role or
	// instance profile. Pass every region enabled in the account: a role is
	// global, and an instance in a region not listed here would be missed.
	// With none, every role and profile re-checks as active.
	Regions []string
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// Grace is how new a resource may be and still be deleted; Grace if 0.
	Grace time.Duration
}

// NewDeleter returns calque's footprint.Deleter, one per resource type:
//
//   - SQS queue: re-checks its messages and workers, then DeleteQueueIfExists;
//   - queue grant (inline policy q-* on a worker role): re-checks that its
//     queue is not active, then DeleteRolePolicy;
//   - instance profile: re-checks that no live instance uses it, then removes
//     its roles and deletes it;
//   - role: re-checks that no live instance uses it and no active queue
//     depends on it, then removes it from its instance profiles (deleting
//     the ones calque owns), deletes its inline policies, detaches its
//     managed policies, and deletes it. It never deletes a managed policy:
//     spawn's (spored-*) and AWS's are only detached;
//   - calque-nfs-sg: re-checks it has no network interfaces, revokes every
//     rule that references it (its own and other groups'), and deletes it;
//   - S3 object: re-checks its age and class, then DeleteObject by key.
//
// Instances re-check as active, and AMIs and snapshots as user data; Delete
// refuses all three. Instances end with their pool, fleet or run; AMIs with
// `calque ami delete`.
func NewDeleter(c Clients, o DeleterOptions) footprint.ByType {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Grace <= 0 {
		o.Grace = Grace
	}
	d := &deleter{c: c, o: o}
	return footprint.ByType{
		TypeQueue:           queueDeleter{d},
		TypeRolePolicy:      grantDeleter{d},
		TypeInstanceProfile: profileDeleter{d},
		TypeRole:            roleDeleter{d},
		TypeSecurityGroup:   sgDeleter{d},
		TypeObject:          objectDeleter{d},
		TypeInstance:        instanceDeleter{d},
		TypeImage:           userDataDeleter{d},
		TypeSnapshot:        userDataDeleter{d},
	}
}

type deleter struct {
	c Clients
	o DeleterOptions
}

func active(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrActive, fmt.Sprintf(format, a...))
}

func notFound(what string) error { return fmt.Errorf("%w: %s", footprint.ErrNotFound, what) }

func (d *deleter) young(t map[string]string, fallback time.Time) bool {
	created, ok := tags.CreatedAt(t)
	if !ok {
		created = fallback
	}
	return !created.IsZero() && d.o.Now().Sub(created) < d.o.Grace
}

// profileUsers returns the live instances, in d.o.Regions, using the
// instance profile profileARN.
func (d *deleter) profileUsers(ctx context.Context, profileARN string) ([]string, error) {
	if len(d.o.Regions) == 0 {
		return nil, errors.New("no regions to check for instances using it")
	}
	var ids []string
	for _, region := range d.o.Regions {
		api, err := d.c.EC2(region)
		if err != nil {
			return nil, err
		}
		ins, err := describeInstances(ctx, api, region, liveInstanceFilters(ec2types.Filter{Name: aws.String("iam-instance-profile.arn"), Values: []string{profileARN}}))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", region, err)
		}
		for _, i := range ins {
			ids = append(ids, i.ID)
		}
	}
	return ids, nil
}

// queueWorkers returns the live instances in region working queue name.
func (d *deleter) queueWorkers(ctx context.Context, region, name string) ([]string, error) {
	api, err := d.c.EC2(region)
	if err != nil {
		return nil, err
	}
	ins, err := describeInstances(ctx, api, region, liveInstanceFilters(ec2types.Filter{Name: aws.String("tag-key"), Values: []string{managedTag}}))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, i := range ins {
		if m := i.Tags[poolModelTag]; m != "" && PoolQueueName(m) == name {
			ids = append(ids, i.ID)
		} else if r := i.Tags[fleetRunTag]; r != "" && RunQueueName(r) == name {
			ids = append(ids, i.ID)
		}
	}
	return ids, nil
}

// queueActivity says why the queue queueARN is active, "" if it is not
// (including when it no longer exists).
func (d *deleter) queueActivity(ctx context.Context, queueARN string) (string, error) {
	parts := strings.Split(queueARN, ":")
	if len(parts) != 6 {
		return "", fmt.Errorf("not a queue ARN: %q", queueARN)
	}
	region, account, name := parts[3], parts[4], parts[5]
	api, err := d.c.SQS(region)
	if err != nil {
		return "", err
	}
	u, err := api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name), QueueOwnerAWSAccountId: aws.String(account)})
	if IsQueueNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	q, err := readQueue(ctx, api, region, aws.ToString(u.QueueUrl))
	if IsQueueNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if n := q.Visible + q.InFlight + q.Delayed; n > 0 {
		return fmt.Sprintf("queue %s has %d message(s)", name, n), nil
	}
	ws, err := d.queueWorkers(ctx, region, name)
	if err != nil {
		return "", err
	}
	if len(ws) > 0 {
		return fmt.Sprintf("queue %s has %d live worker(s)", name, len(ws)), nil
	}
	if d.young(q.Tags, q.Created) {
		return fmt.Sprintf("queue %s was created within the %s grace", name, d.o.Grace), nil
	}
	return "", nil
}

// activeGrant returns a q-* grant on role whose queue is active (or that
// cannot be read), and why; "" if none.
func (d *deleter) activeGrant(ctx context.Context, role string) (string, error) {
	if !isWorkerRole(role) {
		return "", nil
	}
	names, err := listInlinePolicies(ctx, d.c.IAM, role)
	if err != nil {
		return "", err
	}
	for _, p := range names {
		if !matchesName(TypeRolePolicy, p) {
			continue
		}
		why, err := d.grantActivity(ctx, role, p)
		if err != nil || why != "" {
			return why, err
		}
	}
	return "", nil
}

func (d *deleter) grantActivity(ctx context.Context, role, policy string) (string, error) {
	doc, err := d.c.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(policy)})
	if isIAMNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	qs := sqsResources(aws.ToString(doc.PolicyDocument))
	if len(qs) != 1 {
		return fmt.Sprintf("grant %s does not name exactly one queue", policy), nil
	}
	why, err := d.queueActivity(ctx, qs[0])
	if why != "" {
		why = "grant " + policy + ": " + why
	}
	return why, err
}

func isIAMNotFound(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	return err != nil && (errors.As(err, &nse) || errorCode(err) == "NoSuchEntity")
}

// --- SQS queue ---

type queueDeleter struct{ d *deleter }

func (q queueDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	api, err := q.d.c.SQS(r.Region)
	if err != nil {
		return footprint.Resource{}, err
	}
	u, err := api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(r.Name)})
	if IsQueueNotFound(err) {
		return footprint.Resource{}, notFound("queue " + r.Name)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	live, err := readQueue(ctx, api, r.Region, aws.ToString(u.QueueUrl))
	if IsQueueNotFound(err) {
		return footprint.Resource{}, notFound("queue " + r.Name)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	if n := live.Visible + live.InFlight + live.Delayed; n > 0 {
		return footprint.Resource{}, active("%d message(s) queued or in flight", n)
	}
	ws, err := q.d.queueWorkers(ctx, r.Region, r.Name)
	if err != nil {
		return footprint.Resource{}, err
	}
	if len(ws) > 0 {
		return footprint.Resource{}, active("%d live worker(s): %s", len(ws), strings.Join(ws, ", "))
	}
	if q.d.young(live.Tags, live.Created) {
		return footprint.Resource{}, active("created within the %s grace", q.d.o.Grace)
	}
	return footprint.Resource{Name: live.Name, Tags: live.Tags}, nil
}

func (q queueDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	api, err := q.d.c.SQS(r.Region)
	if err != nil {
		return err
	}
	return DeleteQueueIfExists(ctx, api, r.Name)
}

// DeleteQueueIfExists deletes the SQS queue name, treating a queue that is
// already gone as deleted. internal/pool's `calque pool delete` and fleet
// cleanup use it too.
func DeleteQueueIfExists(ctx context.Context, api QueueDeleter, name string) error {
	out, err := api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		if IsQueueNotFound(err) {
			return nil
		}
		return fmt.Errorf("resolve queue %s for deletion: %w", name, err)
	}
	if _, err := api.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl}); err != nil && !IsQueueNotFound(err) {
		return fmt.Errorf("delete queue %s: %w", name, err)
	}
	return nil
}

// --- queue grant (inline policy on a worker role) ---

type grantDeleter struct{ d *deleter }

func (g grantDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	role, policy, ok := splitInlinePolicyID(r.ARN)
	if !ok {
		return footprint.Resource{}, fmt.Errorf("not an inline policy id: %q", r.ARN)
	}
	got, err := g.d.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if isIAMNotFound(err) {
		return footprint.Resource{}, notFound("role " + role)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	why, err := g.d.grantActivity(ctx, role, policy)
	if err != nil {
		return footprint.Resource{}, err
	}
	if why != "" {
		return footprint.Resource{}, active("%s", why)
	}
	if _, err := g.d.c.IAM.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(policy)}); isIAMNotFound(err) {
		return footprint.Resource{}, notFound("inline policy " + policy + " on " + role)
	} else if err != nil {
		return footprint.Resource{}, err
	}
	// An inline policy has no tags of its own; it carries its role's.
	return footprint.Resource{Name: policy, Tags: iamtags.ToMap(got.Role.Tags)}, nil
}

func (g grantDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	role, policy, ok := splitInlinePolicyID(r.ARN)
	if !ok {
		return fmt.Errorf("not an inline policy id: %q", r.ARN)
	}
	_, err := g.d.c.IAM.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(role), PolicyName: aws.String(policy)})
	if isIAMNotFound(err) {
		return notFound("inline policy " + policy + " on " + role)
	}
	return err
}

// --- instance profile ---

type profileDeleter struct{ d *deleter }

func (p profileDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	got, err := p.d.c.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(r.Name)})
	if isIAMNotFound(err) {
		return footprint.Resource{}, notFound("instance profile " + r.Name)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	prof := got.InstanceProfile
	t := iamtags.ToMap(prof.Tags)
	users, err := p.d.profileUsers(ctx, aws.ToString(prof.Arn))
	if err != nil {
		return footprint.Resource{}, active("could not check for instances using it: %v", err)
	}
	if len(users) > 0 {
		return footprint.Resource{}, active("used by %s", strings.Join(users, ", "))
	}
	for _, role := range prof.Roles {
		why, err := p.d.activeGrant(ctx, aws.ToString(role.RoleName))
		if err != nil {
			return footprint.Resource{}, err
		}
		if why != "" {
			return footprint.Resource{}, active("its role %s: %s", aws.ToString(role.RoleName), why)
		}
	}
	if p.d.young(t, aws.ToTime(prof.CreateDate)) {
		return footprint.Resource{}, active("created within the %s grace", p.d.o.Grace)
	}
	return footprint.Resource{Name: aws.ToString(prof.InstanceProfileName), Tags: t}, nil
}

func (p profileDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	got, err := p.d.c.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(r.Name)})
	if isIAMNotFound(err) {
		return notFound("instance profile " + r.Name)
	}
	if err != nil {
		return err
	}
	return deleteProfile(ctx, p.d.c.IAM, got.InstanceProfile)
}

// deleteProfile removes every role from the profile, then deletes it.
func deleteProfile(ctx context.Context, api IAMAPI, prof *iamtypes.InstanceProfile) error {
	name := aws.ToString(prof.InstanceProfileName)
	for _, role := range prof.Roles {
		_, err := api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: role.RoleName})
		if err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("remove role %s from instance profile %s: %w", aws.ToString(role.RoleName), name, err)
		}
	}
	_, err := api.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if isIAMNotFound(err) {
		return notFound("instance profile " + name)
	}
	if err != nil {
		return fmt.Errorf("delete instance profile %s: %w", name, err)
	}
	return nil
}

// --- role ---

type roleDeleter struct{ d *deleter }

func (rd roleDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	got, err := rd.d.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(r.Name)})
	if isIAMNotFound(err) {
		return footprint.Resource{}, notFound("role " + r.Name)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	t := iamtags.ToMap(got.Role.Tags)
	profiles, err := rd.profilesFor(ctx, r.Name)
	if err != nil {
		return footprint.Resource{}, err
	}
	for _, prof := range profiles {
		users, err := rd.d.profileUsers(ctx, aws.ToString(prof.Arn))
		if err != nil {
			return footprint.Resource{}, active("could not check for instances using it: %v", err)
		}
		if len(users) > 0 {
			return footprint.Resource{}, active("used through instance profile %s by %s", aws.ToString(prof.InstanceProfileName), strings.Join(users, ", "))
		}
	}
	why, err := rd.d.activeGrant(ctx, r.Name)
	if err != nil {
		return footprint.Resource{}, err
	}
	if why != "" {
		return footprint.Resource{}, active("%s", why)
	}
	if rd.d.young(t, aws.ToTime(got.Role.CreateDate)) {
		return footprint.Resource{}, active("created within the %s grace", rd.d.o.Grace)
	}
	return footprint.Resource{Name: aws.ToString(got.Role.RoleName), Tags: t}, nil
}

func (rd roleDeleter) profilesFor(ctx context.Context, role string) ([]iamtypes.InstanceProfile, error) {
	var out []iamtypes.InstanceProfile
	var marker *string
	for {
		got, err := rd.d.c.IAM.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(role), Marker: marker})
		if err != nil {
			return nil, fmt.Errorf("list instance profiles for role %s: %w", role, err)
		}
		out = append(out, got.InstanceProfiles...)
		if !got.IsTruncated || got.Marker == nil {
			return out, nil
		}
		marker = got.Marker
	}
}

// Delete takes the role apart in the order IAM requires: out of every
// instance profile (deleting the profiles calque owns), then its inline
// policies, then its managed policies (detached, never deleted), then the
// role.
func (rd roleDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	api := rd.d.c.IAM
	profiles, err := rd.profilesFor(ctx, r.Name)
	if isIAMNotFound(err) {
		return notFound("role " + r.Name)
	}
	if err != nil {
		return err
	}
	for _, prof := range profiles {
		name := aws.ToString(prof.InstanceProfileName)
		got, err := api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
		if err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("read instance profile %s: %w", name, err)
		}
		owned := false
		if err == nil {
			res := footprint.Resource{Type: TypeInstanceProfile, Name: name, Ownership: footprint.OwnershipTagged,
				Class: footprint.ClassLitter, Tags: iamtags.ToMap(got.InstanceProfile.Tags)}
			owned, _ = footprint.Deletable(res, Tool, Patterns())
		}
		if owned {
			if err := deleteProfile(ctx, api, got.InstanceProfile); err != nil && !errors.Is(err, footprint.ErrNotFound) {
				return err
			}
			continue
		}
		// Not calque's: take the role out, leave the profile.
		_, err = api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(name), RoleName: aws.String(r.Name)})
		if err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("remove role %s from instance profile %s: %w", r.Name, name, err)
		}
	}
	inline, err := listInlinePolicies(ctx, api, r.Name)
	if err != nil {
		return err
	}
	for _, p := range inline {
		_, err := api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(r.Name), PolicyName: aws.String(p)})
		if err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("delete inline policy %s from role %s: %w", p, r.Name, err)
		}
	}
	attached, err := listAttached(ctx, api, r.Name)
	if err != nil {
		return err
	}
	for _, arn := range attached {
		// Detach only. A managed policy (spawn's spored-*, AWS's
		// AmazonSSMManagedInstanceCore, or anyone else's) is not calque's to
		// delete; teardown has no DeletePolicy call at all.
		_, err := api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(r.Name), PolicyArn: aws.String(arn)})
		if err != nil && !isIAMNotFound(err) {
			return fmt.Errorf("detach managed policy %s from role %s: %w", arn, r.Name, err)
		}
	}
	_, err = api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(r.Name)})
	if isIAMNotFound(err) {
		return notFound("role " + r.Name)
	}
	if err != nil {
		return fmt.Errorf("delete role %s: %w", r.Name, err)
	}
	return nil
}

// --- calque-nfs-sg ---

type sgDeleter struct{ d *deleter }

func groupID(arn string) string { return arnName(arn) }

func isSGNotFound(err error) bool {
	return err != nil && errorCode(err) == "InvalidGroup.NotFound"
}

func (s sgDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	api, err := s.d.c.EC2(r.Region)
	if err != nil {
		return footprint.Resource{}, err
	}
	id := groupID(r.ARN)
	out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
	if isSGNotFound(err) {
		return footprint.Resource{}, notFound("security group " + id)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	if len(out.SecurityGroups) == 0 {
		return footprint.Resource{}, notFound("security group " + id)
	}
	g := out.SecurityGroups[0]
	enis, err := groupENIs(ctx, api, id)
	if err != nil {
		return footprint.Resource{}, err
	}
	if len(enis) > 0 {
		return footprint.Resource{}, active("used by network interface(s) %s", strings.Join(enis, ", "))
	}
	t := ec2tags.ToMap(g.Tags)
	if s.d.young(t, time.Time{}) {
		return footprint.Resource{}, active("created within the %s grace", s.d.o.Grace)
	}
	return footprint.Resource{Name: aws.ToString(g.GroupName), Tags: t}, nil
}

// Delete revokes every rule that references the group, in the group itself
// (the self-referencing NFS ingress) and in any other group of the account
// (an EFS mount target's group allowing NFS from it), then deletes it. Only
// the rules naming this group are revoked; nothing else in another group is
// touched.
func (s sgDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	api, err := s.d.c.EC2(r.Region)
	if err != nil {
		return err
	}
	id := groupID(r.ARN)
	referrers := map[string]bool{}
	for _, filter := range []string{"ip-permission.group-id", "egress.ip-permission.group-id"} {
		out, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []ec2types.Filter{{Name: aws.String(filter), Values: []string{id}}}})
		if err != nil {
			return fmt.Errorf("find groups referencing %s: %w", id, err)
		}
		for _, g := range out.SecurityGroups {
			referrers[aws.ToString(g.GroupId)] = true
		}
	}
	for g := range referrers {
		if err := revokeReferences(ctx, api, g, id); err != nil {
			return err
		}
	}
	_, err = api.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
	if isSGNotFound(err) {
		return notFound("security group " + id)
	}
	if err != nil {
		return fmt.Errorf("delete security group %s: %w", id, err)
	}
	return nil
}

// revokeReferences revokes the rules of group that reference target.
func revokeReferences(ctx context.Context, api EC2API, group, target string) error {
	var ingress, egress []string
	var token *string
	for {
		out, err := api.DescribeSecurityGroupRules(ctx, &ec2.DescribeSecurityGroupRulesInput{
			Filters:   []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{group}}},
			NextToken: token,
		})
		if err != nil {
			return fmt.Errorf("describe rules of %s: %w", group, err)
		}
		for _, rule := range out.SecurityGroupRules {
			if rule.ReferencedGroupInfo == nil || aws.ToString(rule.ReferencedGroupInfo.GroupId) != target {
				continue
			}
			if aws.ToBool(rule.IsEgress) {
				egress = append(egress, aws.ToString(rule.SecurityGroupRuleId))
			} else {
				ingress = append(ingress, aws.ToString(rule.SecurityGroupRuleId))
			}
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	if len(ingress) > 0 {
		if _, err := api.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(group), SecurityGroupRuleIds: ingress}); err != nil {
			return fmt.Errorf("revoke %s ingress referencing %s: %w", group, target, err)
		}
	}
	if len(egress) > 0 {
		if _, err := api.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(group), SecurityGroupRuleIds: egress}); err != nil {
			return fmt.Errorf("revoke %s egress referencing %s: %w", group, target, err)
		}
	}
	return nil
}

// --- S3 object ---

type objectDeleter struct{ d *deleter }

// bucketKey splits arn:<partition>:s3:::<bucket>/<key>.
func bucketKey(arn string) (bucket, key string, ok bool) {
	_, rest, ok := strings.Cut(arn, ":s3:::")
	if !ok {
		return "", "", false
	}
	bucket, key, ok = strings.Cut(rest, "/")
	return bucket, key, ok && bucket != "" && key != ""
}

func (o objectDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	bucket, key, ok := bucketKey(r.ARN)
	if !ok {
		return footprint.Resource{}, fmt.Errorf("not an S3 object ARN: %q", r.ARN)
	}
	api, _, err := o.d.c.S3(ctx, bucket)
	if err != nil {
		return footprint.Resource{}, err
	}
	head, err := api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if isS3NotFound(err) {
		return footprint.Resource{}, notFound("s3://" + bucket + "/" + key)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	t, err := api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if isS3NotFound(err) {
		return footprint.Resource{}, notFound("s3://" + bucket + "/" + key)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	live := footprint.Resource{Name: key, Tags: s3tags.ToMap(t.TagSet), Class: footprint.ClassLitter}
	switch {
	case isResultKey(key):
		live.Class = footprint.ClassUserData
	case o.d.o.Now().Sub(aws.ToTime(head.LastModified)) < o.d.o.Grace:
		return footprint.Resource{}, active("written within the %s grace", o.d.o.Grace)
	}
	return live, nil
}

func (o objectDeleter) Delete(ctx context.Context, r footprint.Resource) error {
	bucket, key, ok := bucketKey(r.ARN)
	if !ok {
		return fmt.Errorf("not an S3 object ARN: %q", r.ARN)
	}
	api, _, err := o.d.c.S3(ctx, bucket)
	if err != nil {
		return err
	}
	_, err = api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return err
}

// --- instances: never deleted ---

type instanceDeleter struct{ d *deleter }

func (i instanceDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	api, err := i.d.c.EC2(r.Region)
	if err != nil {
		return footprint.Resource{}, err
	}
	ins, err := describeInstances(ctx, api, r.Region, nil, r.Name)
	if err != nil && errorCode(err) == "InvalidInstanceID.NotFound" {
		return footprint.Resource{}, notFound("instance " + r.Name)
	}
	if err != nil {
		return footprint.Resource{}, err
	}
	for _, in := range ins {
		if in.ID == r.Name && in.State != "terminated" {
			return footprint.Resource{}, active("instance is %s; it ends with its run, pool or fleet", in.State)
		}
	}
	return footprint.Resource{}, notFound("instance " + r.Name)
}

func (instanceDeleter) Delete(context.Context, footprint.Resource) error {
	return fmt.Errorf("%w: instances end with their run, `calque pool delete`, or the fleet drain", errNotTeardown)
}

// --- AMIs and snapshots: the user's ---

type userDataDeleter struct{ d *deleter }

func (u userDataDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	api, err := u.d.c.EC2(r.Region)
	if err != nil {
		return footprint.Resource{}, err
	}
	var t map[string]string
	switch r.Type {
	case TypeImage:
		out, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{r.Name}})
		if err != nil || len(out.Images) == 0 {
			if err == nil || errorCode(err) == "InvalidAMIID.NotFound" {
				return footprint.Resource{}, notFound("image " + r.Name)
			}
			return footprint.Resource{}, err
		}
		t = ec2tags.ToMap(out.Images[0].Tags)
	default:
		out, err := api.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{r.Name}})
		if err != nil || len(out.Snapshots) == 0 {
			if err == nil || errorCode(err) == "InvalidSnapshot.NotFound" {
				return footprint.Resource{}, notFound("snapshot " + r.Name)
			}
			return footprint.Resource{}, err
		}
		t = ec2tags.ToMap(out.Snapshots[0].Tags)
	}
	return footprint.Resource{Tags: t, Class: footprint.ClassUserData}, nil
}

func (userDataDeleter) Delete(context.Context, footprint.Resource) error {
	return fmt.Errorf("%w: baked AMIs and their snapshots are deleted only by `calque ami delete`", errNotTeardown)
}
