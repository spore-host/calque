package awstest

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Account is an in-memory AWS account for footprint and teardown tests
// (calque#228): IAM roles, instance profiles and their policies; SQS queues;
// EC2 security groups and their rules, network interfaces, AMIs, snapshots
// and instances; S3 objects and their tags. It answers the read and delete
// calls calquefootprint makes, enforces the dependency rules AWS does
// (DeleteRole with a policy or profile left, DeleteSecurityGroup while a
// rule elsewhere references it or an ENI uses it), and logs every mutating
// call in order, across services, in Ops.
type Account struct {
	ID string

	mu        sync.Mutex
	regions   []string
	roles     map[string]*fakeRole
	profiles  map[string]*fakeProfile
	queues    map[string]map[string]*fakeQueue // region -> name
	groups    map[string]map[string]*fakeGroup // region -> id
	enis      map[string]map[string][]string   // region -> eni -> groups
	images    map[string]map[string]*fakeImage
	snapshots map[string]map[string]map[string]string // region -> id -> tags
	instances map[string]map[string]*fakeInstance
	objects   map[string]map[string]*fakeObject // bucket -> key
	ops       []string
	nextRule  int
}

type fakeRole struct {
	arn      string
	tags     map[string]string
	created  time.Time
	inline   map[string]string
	attached []string
}

type fakeProfile struct {
	arn     string
	tags    map[string]string
	created time.Time
	roles   []string
}

type fakeQueue struct {
	url, arn                   string
	tags                       map[string]string
	created                    time.Time
	visible, inFlight, delayed int
}

type fakeRule struct {
	id     string
	egress bool
	ref    string
}

type fakeGroup struct {
	name, vpc string
	tags      map[string]string
	rules     []fakeRule
}

type fakeImage struct {
	tags      map[string]string
	snapshots []string
}

type fakeInstance struct {
	state      string
	tags       map[string]string
	profileARN string
}

type fakeObject struct {
	tags     map[string]string
	modified time.Time
}

// NewAccount returns an empty account with the given enabled regions.
func NewAccount(id string, regions ...string) *Account {
	return &Account{
		ID: id, regions: regions,
		roles: map[string]*fakeRole{}, profiles: map[string]*fakeProfile{},
		queues: map[string]map[string]*fakeQueue{}, groups: map[string]map[string]*fakeGroup{},
		enis: map[string]map[string][]string{}, images: map[string]map[string]*fakeImage{},
		snapshots: map[string]map[string]map[string]string{}, instances: map[string]map[string]*fakeInstance{},
		objects: map[string]map[string]*fakeObject{},
	}
}

// Config is an aws.Config whose IAM, SQS, EC2, S3 and STS calls this account
// answers.
func (a *Account) Config() aws.Config {
	return ConfigRegional(RegionalServices{
		"IAM": a.handleIAM, "SQS": a.handleSQS, "EC2": a.handleEC2, "S3": a.handleS3,
		"STS": func(_, op string, in any) (any, error) { return STS(a.ID)(op, in) },
	})
}

// --- seeding ---

// AddRole adds a role with its same-named instance profile (as spawn creates
// them), inline policies and attached managed policies.
func (a *Account) AddRole(name string, tags map[string]string, created time.Time, inline map[string]string, attached ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if inline == nil {
		inline = map[string]string{}
	}
	a.roles[name] = &fakeRole{arn: "arn:aws:iam::" + a.ID + ":role/" + name, tags: clone(tags), created: created, inline: inline, attached: attached}
	a.profiles[name] = &fakeProfile{arn: a.ProfileARN(name), tags: clone(tags), created: created, roles: []string{name}}
}

// ProfileARN is the ARN of the instance profile name.
func (a *Account) ProfileARN(name string) string {
	return "arn:aws:iam::" + a.ID + ":instance-profile/" + name
}

// QueueARN is the ARN of queue name in region.
func (a *Account) QueueARN(region, name string) string {
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, a.ID, name)
}

// AddQueue adds a queue with the given backlog.
func (a *Account) AddQueue(region, name string, tags map[string]string, created time.Time, visible, inFlight int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.queues[region] == nil {
		a.queues[region] = map[string]*fakeQueue{}
	}
	a.queues[region][name] = &fakeQueue{
		url: fmt.Sprintf("https://sqs.%s.amazonaws.com/%s/%s", region, a.ID, name), arn: a.QueueARN(region, name),
		tags: clone(tags), created: created, visible: visible, inFlight: inFlight,
	}
}

// AddSecurityGroup adds a security group with a self-referencing ingress rule.
func (a *Account) AddSecurityGroup(region, id, name, vpc string, tags map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.groups[region] == nil {
		a.groups[region] = map[string]*fakeGroup{}
	}
	a.groups[region][id] = &fakeGroup{name: name, vpc: vpc, tags: clone(tags)}
	a.addRuleLocked(region, id, false, id)
}

// AddRule adds a rule to group that references ref.
func (a *Account) AddRule(region, group string, egress bool, ref string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addRuleLocked(region, group, egress, ref)
}

func (a *Account) addRuleLocked(region, group string, egress bool, ref string) {
	a.nextRule++
	g := a.groups[region][group]
	g.rules = append(g.rules, fakeRule{id: fmt.Sprintf("sgr-%04d", a.nextRule), egress: egress, ref: ref})
}

// AddENI adds a network interface in the given groups.
func (a *Account) AddENI(region, id string, groups ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enis[region] == nil {
		a.enis[region] = map[string][]string{}
	}
	a.enis[region][id] = groups
}

// AddImage adds an AMI and its snapshots, all with tags.
func (a *Account) AddImage(region, id string, tags map[string]string, snapshots ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.images[region] == nil {
		a.images[region] = map[string]*fakeImage{}
		a.snapshots[region] = map[string]map[string]string{}
	}
	a.images[region][id] = &fakeImage{tags: clone(tags), snapshots: snapshots}
	for _, s := range snapshots {
		a.snapshots[region][s] = clone(tags)
	}
}

// AddInstance adds an instance.
func (a *Account) AddInstance(region, id, state string, tags map[string]string, profileARN string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.instances[region] == nil {
		a.instances[region] = map[string]*fakeInstance{}
	}
	a.instances[region][id] = &fakeInstance{state: state, tags: clone(tags), profileARN: profileARN}
}

// AddObject adds an S3 object.
func (a *Account) AddObject(bucket, key string, tags map[string]string, modified time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.objects[bucket] == nil {
		a.objects[bucket] = map[string]*fakeObject{}
	}
	a.objects[bucket][key] = &fakeObject{tags: clone(tags), modified: modified}
}

// SetRoleTags replaces a role's tags (to simulate a change after a plan).
func (a *Account) SetRoleTags(name string, tags map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.roles[name].tags = clone(tags)
}

// SetQueueBacklog sets a queue's visible messages.
func (a *Account) SetQueueBacklog(region, name string, visible int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queues[region][name].visible = visible
}

// --- inspection ---

// Ops returns the mutating calls so far, in order, as "Service:Operation
// target" strings.
func (a *Account) Ops() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ops...)
}

// HasRole, HasProfile, HasQueue, HasGroup and HasObject report whether the
// resource still exists.
func (a *Account) HasRole(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.roles[name] != nil
}

// HasProfile reports whether the instance profile exists.
func (a *Account) HasProfile(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.profiles[name] != nil
}

// HasQueue reports whether the queue exists.
func (a *Account) HasQueue(region, name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queues[region][name] != nil
}

// HasGroup reports whether the security group exists.
func (a *Account) HasGroup(region, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.groups[region][id] != nil
}

// HasObject reports whether the object exists.
func (a *Account) HasObject(bucket, key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.objects[bucket][key] != nil
}

// InlinePolicies returns a role's inline policy names, sorted.
func (a *Account) InlinePolicies(role string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.roles[role]
	if r == nil {
		return nil
	}
	return sortedNames(r.inline)
}

// Rules returns a group's rules as "ingress|egress:<ref>" strings.
func (a *Account) Rules(region, group string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.groups[region][group].rules {
		dir := "ingress"
		if r.egress {
			dir = "egress"
		}
		out = append(out, dir+":"+r.ref)
	}
	return out
}

func (a *Account) op(s string) { a.ops = append(a.ops, s) }

// --- IAM ---

func iamNotFound(what string) error {
	return &iamtypes.NoSuchEntityException{Message: aws.String(what + " not found")}
}

func (a *Account) handleIAM(_, op string, input any) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch in := input.(type) {
	case *iam.ListRolesInput:
		out := &iam.ListRolesOutput{}
		for _, n := range sortedNames(a.roles) {
			out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(n), Arn: aws.String(a.roles[n].arn)})
		}
		return out, nil
	case *iam.GetRoleInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil {
			return nil, iamNotFound("role " + aws.ToString(in.RoleName))
		}
		return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Arn: aws.String(r.arn), Tags: toIAMTags(r.tags), CreateDate: aws.Time(r.created)}}, nil
	case *iam.ListRolePoliciesInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil {
			return nil, iamNotFound("role " + aws.ToString(in.RoleName))
		}
		return &iam.ListRolePoliciesOutput{PolicyNames: sortedNames(r.inline)}, nil
	case *iam.GetRolePolicyInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil {
			return nil, iamNotFound("role " + aws.ToString(in.RoleName))
		}
		doc, ok := r.inline[aws.ToString(in.PolicyName)]
		if !ok {
			return nil, iamNotFound("policy " + aws.ToString(in.PolicyName))
		}
		// IAM returns the document URL-encoded.
		return &iam.GetRolePolicyOutput{RoleName: in.RoleName, PolicyName: in.PolicyName, PolicyDocument: aws.String(url.PathEscape(doc))}, nil
	case *iam.ListAttachedRolePoliciesInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil {
			return nil, iamNotFound("role " + aws.ToString(in.RoleName))
		}
		out := &iam.ListAttachedRolePoliciesOutput{}
		for _, p := range r.attached {
			out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(p), PolicyName: aws.String(p[strings.LastIndex(p, "/")+1:])})
		}
		return out, nil
	case *iam.ListInstanceProfilesInput:
		out := &iam.ListInstanceProfilesOutput{}
		for _, n := range sortedNames(a.profiles) {
			out.InstanceProfiles = append(out.InstanceProfiles, a.profileLocked(n, false))
		}
		return out, nil
	case *iam.GetInstanceProfileInput:
		if a.profiles[aws.ToString(in.InstanceProfileName)] == nil {
			return nil, iamNotFound("instance profile " + aws.ToString(in.InstanceProfileName))
		}
		p := a.profileLocked(aws.ToString(in.InstanceProfileName), true)
		return &iam.GetInstanceProfileOutput{InstanceProfile: &p}, nil
	case *iam.ListInstanceProfilesForRoleInput:
		role := aws.ToString(in.RoleName)
		if a.roles[role] == nil {
			return nil, iamNotFound("role " + role)
		}
		out := &iam.ListInstanceProfilesForRoleOutput{}
		for _, n := range sortedNames(a.profiles) {
			for _, r := range a.profiles[n].roles {
				if r == role {
					out.InstanceProfiles = append(out.InstanceProfiles, a.profileLocked(n, false))
				}
			}
		}
		return out, nil
	case *iam.DeleteRolePolicyInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil || r.inline[aws.ToString(in.PolicyName)] == "" {
			return nil, iamNotFound("policy " + aws.ToString(in.PolicyName))
		}
		delete(r.inline, aws.ToString(in.PolicyName))
		a.op("IAM:DeleteRolePolicy " + aws.ToString(in.RoleName) + "/" + aws.ToString(in.PolicyName))
		return &iam.DeleteRolePolicyOutput{}, nil
	case *iam.DetachRolePolicyInput:
		r := a.roles[aws.ToString(in.RoleName)]
		if r == nil {
			return nil, iamNotFound("role " + aws.ToString(in.RoleName))
		}
		kept := r.attached[:0]
		found := false
		for _, p := range r.attached {
			if p == aws.ToString(in.PolicyArn) {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if !found {
			return nil, iamNotFound("attachment " + aws.ToString(in.PolicyArn))
		}
		r.attached = kept
		a.op("IAM:DetachRolePolicy " + aws.ToString(in.RoleName) + " " + aws.ToString(in.PolicyArn))
		return &iam.DetachRolePolicyOutput{}, nil
	case *iam.RemoveRoleFromInstanceProfileInput:
		p := a.profiles[aws.ToString(in.InstanceProfileName)]
		if p == nil {
			return nil, iamNotFound("instance profile " + aws.ToString(in.InstanceProfileName))
		}
		var kept []string
		for _, r := range p.roles {
			if r != aws.ToString(in.RoleName) {
				kept = append(kept, r)
			}
		}
		p.roles = kept
		a.op("IAM:RemoveRoleFromInstanceProfile " + aws.ToString(in.InstanceProfileName) + " " + aws.ToString(in.RoleName))
		return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
	case *iam.DeleteInstanceProfileInput:
		name := aws.ToString(in.InstanceProfileName)
		p := a.profiles[name]
		if p == nil {
			return nil, iamNotFound("instance profile " + name)
		}
		if len(p.roles) > 0 {
			return nil, &iamtypes.DeleteConflictException{Message: aws.String("instance profile " + name + " still has roles")}
		}
		delete(a.profiles, name)
		a.op("IAM:DeleteInstanceProfile " + name)
		return &iam.DeleteInstanceProfileOutput{}, nil
	case *iam.DeleteRoleInput:
		name := aws.ToString(in.RoleName)
		r := a.roles[name]
		if r == nil {
			return nil, iamNotFound("role " + name)
		}
		if len(r.inline) > 0 || len(r.attached) > 0 {
			return nil, &iamtypes.DeleteConflictException{Message: aws.String("role " + name + " still has policies")}
		}
		for _, p := range a.profiles {
			for _, pr := range p.roles {
				if pr == name {
					return nil, &iamtypes.DeleteConflictException{Message: aws.String("role " + name + " is in an instance profile")}
				}
			}
		}
		delete(a.roles, name)
		a.op("IAM:DeleteRole " + name)
		return &iam.DeleteRoleOutput{}, nil
	}
	a.op("IAM:UNMODELLED " + op)
	return nil, Unhandled("IAM", op)
}

func (a *Account) profileLocked(name string, withTags bool) iamtypes.InstanceProfile {
	p := a.profiles[name]
	out := iamtypes.InstanceProfile{InstanceProfileName: aws.String(name), Arn: aws.String(p.arn), CreateDate: aws.Time(p.created)}
	if withTags {
		out.Tags = toIAMTags(p.tags)
	}
	for _, r := range p.roles {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(r)})
	}
	return out
}

// --- SQS ---

func (a *Account) queueByURL(region, u string) *fakeQueue {
	for _, q := range a.queues[region] {
		if q.url == u {
			return q
		}
	}
	return nil
}

func (a *Account) handleSQS(region, op string, input any) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	noQueue := &sqstypes.QueueDoesNotExist{Message: aws.String("no such queue")}
	switch in := input.(type) {
	case *sqs.ListQueuesInput:
		out := &sqs.ListQueuesOutput{}
		for _, n := range sortedNames(a.queues[region]) {
			if strings.HasPrefix(n, aws.ToString(in.QueueNamePrefix)) {
				out.QueueUrls = append(out.QueueUrls, a.queues[region][n].url)
			}
		}
		return out, nil
	case *sqs.GetQueueUrlInput:
		q := a.queues[region][aws.ToString(in.QueueName)]
		if q == nil {
			return nil, noQueue
		}
		return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(q.url)}, nil
	case *sqs.GetQueueAttributesInput:
		q := a.queueByURL(region, aws.ToString(in.QueueUrl))
		if q == nil {
			return nil, noQueue
		}
		return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
			"QueueArn":                              q.arn,
			"CreatedTimestamp":                      fmt.Sprint(q.created.Unix()),
			"ApproximateNumberOfMessages":           fmt.Sprint(q.visible),
			"ApproximateNumberOfMessagesNotVisible": fmt.Sprint(q.inFlight),
			"ApproximateNumberOfMessagesDelayed":    fmt.Sprint(q.delayed),
		}}, nil
	case *sqs.ListQueueTagsInput:
		q := a.queueByURL(region, aws.ToString(in.QueueUrl))
		if q == nil {
			return nil, noQueue
		}
		return &sqs.ListQueueTagsOutput{Tags: clone(q.tags)}, nil
	case *sqs.DeleteQueueInput:
		for n, q := range a.queues[region] {
			if q.url == aws.ToString(in.QueueUrl) {
				delete(a.queues[region], n)
				a.op("SQS:DeleteQueue " + n)
				return &sqs.DeleteQueueOutput{}, nil
			}
		}
		return nil, noQueue
	}
	a.op("SQS:UNMODELLED " + op)
	return nil, Unhandled("SQS", op)
}

// --- EC2 ---

func filterValues(fs []ec2types.Filter, name string) ([]string, bool) {
	for _, f := range fs {
		if aws.ToString(f.Name) == name {
			return f.Values, true
		}
	}
	return nil, false
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func (a *Account) handleEC2(region, op string, input any) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch in := input.(type) {
	case *ec2.DescribeRegionsInput:
		out := &ec2.DescribeRegionsOutput{}
		for _, r := range a.regions {
			out.Regions = append(out.Regions, ec2types.Region{RegionName: aws.String(r)})
		}
		return out, nil
	case *ec2.DescribeInstancesInput:
		var insts []ec2types.Instance
		for _, id := range sortedNames(a.instances[region]) {
			i := a.instances[region][id]
			if len(in.InstanceIds) > 0 && !contains(in.InstanceIds, id) {
				continue
			}
			if !matchInstance(in.Filters, i) {
				continue
			}
			inst := ec2types.Instance{InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: ec2types.InstanceStateName(i.state)}, Tags: toEC2Tags(i.tags)}
			if i.profileARN != "" {
				inst.IamInstanceProfile = &ec2types.IamInstanceProfile{Arn: aws.String(i.profileARN)}
			}
			insts = append(insts, inst)
		}
		if len(in.InstanceIds) > 0 && len(insts) == 0 {
			return nil, &APIError{Code: "InvalidInstanceID.NotFound", Message: "no such instance"}
		}
		out := &ec2.DescribeInstancesOutput{}
		if len(insts) > 0 {
			out.Reservations = []ec2types.Reservation{{Instances: insts}}
		}
		return out, nil
	case *ec2.DescribeSecurityGroupsInput:
		out := &ec2.DescribeSecurityGroupsOutput{}
		ids := in.GroupIds
		for _, id := range ids {
			if a.groups[region][id] == nil {
				return nil, &APIError{Code: "InvalidGroup.NotFound", Message: "no such group " + id}
			}
		}
		for _, id := range sortedNames(a.groups[region]) {
			g := a.groups[region][id]
			if len(ids) > 0 && !contains(ids, id) {
				continue
			}
			if v, ok := filterValues(in.Filters, "group-name"); ok && !contains(v, g.name) {
				continue
			}
			if v, ok := filterValues(in.Filters, "ip-permission.group-id"); ok && !g.references(v, false) {
				continue
			}
			if v, ok := filterValues(in.Filters, "egress.ip-permission.group-id"); ok && !g.references(v, true) {
				continue
			}
			out.SecurityGroups = append(out.SecurityGroups, ec2types.SecurityGroup{GroupId: aws.String(id), GroupName: aws.String(g.name), VpcId: aws.String(g.vpc), Tags: toEC2Tags(g.tags)})
		}
		return out, nil
	case *ec2.DescribeNetworkInterfacesInput:
		v, _ := filterValues(in.Filters, "group-id")
		out := &ec2.DescribeNetworkInterfacesOutput{}
		for _, id := range sortedNames(a.enis[region]) {
			for _, g := range a.enis[region][id] {
				if contains(v, g) {
					out.NetworkInterfaces = append(out.NetworkInterfaces, ec2types.NetworkInterface{NetworkInterfaceId: aws.String(id)})
					break
				}
			}
		}
		return out, nil
	case *ec2.DescribeSecurityGroupRulesInput:
		v, _ := filterValues(in.Filters, "group-id")
		out := &ec2.DescribeSecurityGroupRulesOutput{}
		for _, gid := range v {
			g := a.groups[region][gid]
			if g == nil {
				continue
			}
			for _, r := range g.rules {
				out.SecurityGroupRules = append(out.SecurityGroupRules, ec2types.SecurityGroupRule{
					SecurityGroupRuleId: aws.String(r.id), GroupId: aws.String(gid), IsEgress: aws.Bool(r.egress),
					ReferencedGroupInfo: &ec2types.ReferencedSecurityGroup{GroupId: aws.String(r.ref)},
				})
			}
		}
		return out, nil
	case *ec2.RevokeSecurityGroupIngressInput:
		return &ec2.RevokeSecurityGroupIngressOutput{}, a.revoke(region, aws.ToString(in.GroupId), in.SecurityGroupRuleIds, false)
	case *ec2.RevokeSecurityGroupEgressInput:
		return &ec2.RevokeSecurityGroupEgressOutput{}, a.revoke(region, aws.ToString(in.GroupId), in.SecurityGroupRuleIds, true)
	case *ec2.DeleteSecurityGroupInput:
		id := aws.ToString(in.GroupId)
		if a.groups[region][id] == nil {
			return nil, &APIError{Code: "InvalidGroup.NotFound", Message: "no such group " + id}
		}
		for _, groups := range a.enis[region] {
			if contains(groups, id) {
				return nil, &APIError{Code: "DependencyViolation", Message: id + " is in use by a network interface"}
			}
		}
		for gid, g := range a.groups[region] {
			if gid == id {
				continue
			}
			for _, r := range g.rules {
				if r.ref == id {
					return nil, &APIError{Code: "DependencyViolation", Message: id + " is referenced by " + gid}
				}
			}
		}
		delete(a.groups[region], id)
		a.op("EC2:DeleteSecurityGroup " + id)
		return &ec2.DeleteSecurityGroupOutput{}, nil
	case *ec2.DescribeImagesInput:
		out := &ec2.DescribeImagesOutput{}
		for _, id := range sortedNames(a.images[region]) {
			im := a.images[region][id]
			if len(in.ImageIds) > 0 && !contains(in.ImageIds, id) {
				continue
			}
			if v, ok := filterValues(in.Filters, "tag-key"); ok && !hasAnyKey(im.tags, v) {
				continue
			}
			img := ec2types.Image{ImageId: aws.String(id), Name: aws.String(id + "-name"), State: ec2types.ImageStateAvailable, Tags: toEC2Tags(im.tags)}
			for _, s := range im.snapshots {
				img.BlockDeviceMappings = append(img.BlockDeviceMappings, ec2types.BlockDeviceMapping{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String(s)}})
			}
			out.Images = append(out.Images, img)
		}
		return out, nil
	case *ec2.DescribeSnapshotsInput:
		out := &ec2.DescribeSnapshotsOutput{}
		for _, id := range sortedNames(a.snapshots[region]) {
			t := a.snapshots[region][id]
			if len(in.SnapshotIds) > 0 && !contains(in.SnapshotIds, id) {
				continue
			}
			if v, ok := filterValues(in.Filters, "tag-key"); ok && !hasAnyKey(t, v) {
				continue
			}
			out.Snapshots = append(out.Snapshots, ec2types.Snapshot{SnapshotId: aws.String(id), Tags: toEC2Tags(t)})
		}
		return out, nil
	}
	a.op("EC2:UNMODELLED " + op)
	return nil, Unhandled("EC2", op)
}

func (g *fakeGroup) references(ids []string, egress bool) bool {
	for _, r := range g.rules {
		if r.egress == egress && contains(ids, r.ref) {
			return true
		}
	}
	return false
}

func (a *Account) revoke(region, group string, ids []string, egress bool) error {
	g := a.groups[region][group]
	if g == nil {
		return &APIError{Code: "InvalidGroup.NotFound", Message: "no such group " + group}
	}
	var kept []fakeRule
	for _, r := range g.rules {
		if r.egress == egress && contains(ids, r.id) {
			dir := "Ingress"
			if egress {
				dir = "Egress"
			}
			a.op("EC2:RevokeSecurityGroup" + dir + " " + group + " " + r.ref)
			continue
		}
		kept = append(kept, r)
	}
	g.rules = kept
	return nil
}

func matchInstance(fs []ec2types.Filter, i *fakeInstance) bool {
	for _, f := range fs {
		name := aws.ToString(f.Name)
		switch {
		case name == "instance-state-name":
			if !contains(f.Values, i.state) {
				return false
			}
		case name == "iam-instance-profile.arn":
			if !contains(f.Values, i.profileARN) {
				return false
			}
		case name == "tag-key":
			if !hasAnyKey(i.tags, f.Values) {
				return false
			}
		case strings.HasPrefix(name, "tag:"):
			v, ok := i.tags[strings.TrimPrefix(name, "tag:")]
			if !ok || !contains(f.Values, v) {
				return false
			}
		}
	}
	return true
}

func hasAnyKey(m map[string]string, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// --- S3 ---

func (a *Account) handleS3(_, op string, input any) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	noKey := &s3types.NoSuchKey{Message: aws.String("no such key")}
	switch in := input.(type) {
	case *s3.HeadBucketInput:
		return &s3.HeadBucketOutput{}, nil
	case *s3.ListObjectsV2Input:
		out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
		objs := a.objects[aws.ToString(in.Bucket)]
		for _, k := range sortedNames(objs) {
			if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
				out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), LastModified: aws.Time(objs[k].modified), Size: aws.Int64(1)})
			}
		}
		return out, nil
	case *s3.GetObjectTaggingInput:
		o := a.objects[aws.ToString(in.Bucket)][aws.ToString(in.Key)]
		if o == nil {
			return nil, noKey
		}
		out := &s3.GetObjectTaggingOutput{}
		for _, k := range sortedNames(o.tags) {
			out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(o.tags[k])})
		}
		return out, nil
	case *s3.HeadObjectInput:
		o := a.objects[aws.ToString(in.Bucket)][aws.ToString(in.Key)]
		if o == nil {
			return nil, &APIError{Code: "NotFound", Message: "no such key"}
		}
		return &s3.HeadObjectOutput{LastModified: aws.Time(o.modified)}, nil
	case *s3.DeleteObjectInput:
		delete(a.objects[aws.ToString(in.Bucket)], aws.ToString(in.Key))
		a.op("S3:DeleteObject " + aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key))
		return &s3.DeleteObjectOutput{}, nil
	}
	a.op("S3:UNMODELLED " + op)
	return nil, Unhandled("S3", op)
}

// --- helpers ---

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toEC2Tags(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for _, k := range sortedNames(m) {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}
