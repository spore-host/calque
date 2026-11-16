package awstest

import (
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// STS answers GetCallerIdentity with account.
func STS(account string) Handler {
	return func(op string, _ any) (any, error) {
		if op != "GetCallerIdentity" {
			return nil, Unhandled("STS", op)
		}
		return &sts.GetCallerIdentityOutput{
			Account: aws.String(account),
			Arn:     aws.String("arn:aws:iam::" + account + ":user/calque-test"),
			UserId:  aws.String("AIDATEST"),
		}, nil
	}
}

// IAM is an in-memory IAM covering the calls spawn v0.131.0's
// CreateOrGetInstanceProfile and calque's own role tagging and per-queue
// grants make: roles, instance profiles, their tags, and inline policies.
type IAM struct {
	mu       sync.Mutex
	roles    map[string]*iamtypes.Role
	profiles map[string]*iamtypes.InstanceProfile
	policies map[string]map[string]string // role -> policy name -> document
	ops      []string
}

// NewIAM returns an empty IAM.
func NewIAM() *IAM {
	return &IAM{
		roles:    map[string]*iamtypes.Role{},
		profiles: map[string]*iamtypes.InstanceProfile{},
		policies: map[string]map[string]string{},
	}
}

// SeedRole adds an existing role and its same-named instance profile with
// the given tags, as an older calque (or spawn) would have left them.
func (f *IAM) SeedRole(name string, tags map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[name] = &iamtypes.Role{RoleName: aws.String(name), Tags: toIAMTags(tags)}
	f.profiles[name] = &iamtypes.InstanceProfile{InstanceProfileName: aws.String(name), Tags: toIAMTags(tags)}
}

// RoleTags returns the role's tags (nil if there is no such role).
func (f *IAM) RoleTags(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.roles[name]; ok {
		return fromIAMTags(r.Tags)
	}
	return nil
}

// ProfileTags returns the instance profile's tags (nil if there is none).
func (f *IAM) ProfileTags(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.profiles[name]; ok {
		return fromIAMTags(p.Tags)
	}
	return nil
}

// Ops returns the operations seen so far, in order.
func (f *IAM) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func noSuchEntity(what string) error {
	return &iamtypes.NoSuchEntityException{Message: aws.String(what + " not found")}
}

// Handle is the IAM Handler.
func (f *IAM) Handle(op string, input any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	switch in := input.(type) {
	case *iam.GetRoleInput:
		r, ok := f.roles[aws.ToString(in.RoleName)]
		if !ok {
			return nil, noSuchEntity("role " + aws.ToString(in.RoleName))
		}
		cp := *r
		return &iam.GetRoleOutput{Role: &cp}, nil
	case *iam.CreateRoleInput:
		name := aws.ToString(in.RoleName)
		if _, ok := f.roles[name]; ok {
			return nil, &iamtypes.EntityAlreadyExistsException{Message: aws.String("role " + name)}
		}
		f.roles[name] = &iamtypes.Role{RoleName: in.RoleName, Tags: append([]iamtypes.Tag(nil), in.Tags...)}
		return &iam.CreateRoleOutput{Role: f.roles[name]}, nil
	case *iam.TagRoleInput:
		r, ok := f.roles[aws.ToString(in.RoleName)]
		if !ok {
			return nil, noSuchEntity("role " + aws.ToString(in.RoleName))
		}
		r.Tags = mergeIAMTags(r.Tags, in.Tags)
		return &iam.TagRoleOutput{}, nil
	case *iam.GetInstanceProfileInput:
		p, ok := f.profiles[aws.ToString(in.InstanceProfileName)]
		if !ok {
			return nil, noSuchEntity("instance profile " + aws.ToString(in.InstanceProfileName))
		}
		cp := *p
		return &iam.GetInstanceProfileOutput{InstanceProfile: &cp}, nil
	case *iam.CreateInstanceProfileInput:
		name := aws.ToString(in.InstanceProfileName)
		if _, ok := f.profiles[name]; ok {
			return nil, &iamtypes.EntityAlreadyExistsException{Message: aws.String("instance profile " + name)}
		}
		f.profiles[name] = &iamtypes.InstanceProfile{InstanceProfileName: in.InstanceProfileName, Tags: append([]iamtypes.Tag(nil), in.Tags...)}
		return &iam.CreateInstanceProfileOutput{InstanceProfile: f.profiles[name]}, nil
	case *iam.TagInstanceProfileInput:
		p, ok := f.profiles[aws.ToString(in.InstanceProfileName)]
		if !ok {
			return nil, noSuchEntity("instance profile " + aws.ToString(in.InstanceProfileName))
		}
		p.Tags = mergeIAMTags(p.Tags, in.Tags)
		return &iam.TagInstanceProfileOutput{}, nil
	case *iam.AddRoleToInstanceProfileInput:
		return &iam.AddRoleToInstanceProfileOutput{}, nil
	case *iam.AttachRolePolicyInput:
		return &iam.AttachRolePolicyOutput{}, nil
	case *iam.PutRolePolicyInput:
		role := aws.ToString(in.RoleName)
		if f.policies[role] == nil {
			f.policies[role] = map[string]string{}
		}
		f.policies[role][aws.ToString(in.PolicyName)] = aws.ToString(in.PolicyDocument)
		return &iam.PutRolePolicyOutput{}, nil
	case *iam.GetRolePolicyInput:
		doc, ok := f.policies[aws.ToString(in.RoleName)][aws.ToString(in.PolicyName)]
		if !ok {
			return nil, noSuchEntity("policy " + aws.ToString(in.PolicyName))
		}
		return &iam.GetRolePolicyOutput{RoleName: in.RoleName, PolicyName: in.PolicyName, PolicyDocument: aws.String(doc)}, nil
	case *iam.DeleteRolePolicyInput:
		delete(f.policies[aws.ToString(in.RoleName)], aws.ToString(in.PolicyName))
		return &iam.DeleteRolePolicyOutput{}, nil
	case *iam.ListRolePoliciesInput:
		var names []string
		for n := range f.policies[aws.ToString(in.RoleName)] {
			names = append(names, n)
		}
		return &iam.ListRolePoliciesOutput{PolicyNames: names}, nil
	}
	return nil, Unhandled("IAM", op)
}

func toIAMTags(m map[string]string) []iamtypes.Tag {
	out := make([]iamtypes.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func fromIAMTags(ts []iamtypes.Tag) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func mergeIAMTags(have, add []iamtypes.Tag) []iamtypes.Tag {
	m := fromIAMTags(have)
	for k, v := range fromIAMTags(add) {
		m[k] = v
	}
	return toIAMTags(m)
}
