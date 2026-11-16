package calquefootprint

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
)

// Tool is calque's spore-host:managed-by value.
const Tool = tags.ToolCalque

// Grace is how long a resource must have been idle, or exist, before the
// footprint calls it litter: the leave-no-trace grace (spore-host/spore-host#752).
const Grace = 24 * time.Hour

// The names calque gives what it creates. internal/pool and internal/plan
// build their names from these, so the footprint and the code that creates
// the resources cannot drift apart.
const (
	// PoolQueuePrefix starts every pool queue's name: calque-pool-<slug(model)>.
	PoolQueuePrefix = "calque-pool-"
	// FleetQueuePrefix starts every fleet run queue's name:
	// calque-fleet-<slug(run id)>.
	FleetQueuePrefix = "calque-fleet-"
	// RealRunRolePrefix starts the per-bucket real-run role's name:
	// calque-real-run-<16 hex>.
	RealRunRolePrefix = "calque-real-run-"
	// PoolWorkerRole is the shared role every pool's workers run under.
	PoolWorkerRole = "calque-pool-worker"
	// FleetWorkerRole is the shared role every fleet run's workers run under.
	FleetWorkerRole = "calque-fleet-worker"
	// QueueGrantPrefix starts a per-queue inline grant on a worker role
	// (calque#226): q-<12 hex of SHA-256(queue ARN)>.
	QueueGrantPrefix = "q-"
	// NFSSecurityGroupName is the per-VPC security group calque creates for
	// EFS mounts.
	NFSSecurityGroupName = "calque-nfs-sg"
	// BakedImageTag marks an AMI (and its snapshots) baked by `calque ami bake`.
	BakedImageTag = "calque:baked-image"
)

// The tag keys the footprint reads besides libs/tags'. They match
// internal/calquetags.
const (
	managedTag   = "calque:managed"
	poolModelTag = "calque:pool-model"
	fleetRunTag  = "calque:fleet-run"
)

// S3Prefixes are the key prefixes calque writes under in a caller's bucket:
// real runs (runs/<run-id>/), fleet runs (fleet/<run-id>/) and AMI bakes
// (ami-bake/<run-id>/). The footprint looks at nothing else in a bucket.
var S3Prefixes = []string{"runs/", "fleet/", "ami-bake/"}

// Resource types, as Resource.Type. IAM, SQS and EC2 security groups and
// instances use their CloudFormation type names. An inline role policy uses
// AWS::IAM::RolePolicy. AMIs, snapshots and S3 objects have no CloudFormation
// type; they use AWS::EC2::Image, AWS::EC2::Snapshot and AWS::S3::Object.
const (
	TypeRole            = "AWS::IAM::Role"
	TypeInstanceProfile = "AWS::IAM::InstanceProfile"
	TypeRolePolicy      = "AWS::IAM::RolePolicy"
	TypeQueue           = "AWS::SQS::Queue"
	TypeSecurityGroup   = "AWS::EC2::SecurityGroup"
	TypeImage           = "AWS::EC2::Image"
	TypeSnapshot        = "AWS::EC2::Snapshot"
	TypeInstance        = "AWS::EC2::Instance"
	TypeObject          = "AWS::S3::Object"
)

// RegionGlobal is Resource.Region for IAM resources.
const RegionGlobal = "global"

// inlinePolicySep joins a role ARN and an inline policy name into the
// identifier of an inline policy, which has no ARN of its own:
// <role ARN>#inline-policy/<policy name>.
const inlinePolicySep = "#inline-policy/"

// InlinePolicyID is the Resource.ARN of an inline policy on a role.
func InlinePolicyID(roleARN, policy string) string { return roleARN + inlinePolicySep + policy }

// splitInlinePolicyID returns the role name and policy name of an
// InlinePolicyID.
func splitInlinePolicyID(id string) (role, policy string, ok bool) {
	roleARN, policy, ok := strings.Cut(id, inlinePolicySep)
	if !ok || policy == "" {
		return "", "", false
	}
	i := strings.LastIndex(roleARN, "/")
	if i < 0 || i == len(roleARN)-1 {
		return "", "", false
	}
	return roleARN[i+1:], policy, true
}

// Patterns are calque's registered resource names, for footprint.Deletable.
func Patterns() []footprint.NamePattern {
	roles := `calque-real-run-[0-9a-f]{16}|calque-pool-worker|calque-fleet-worker`
	return []footprint.NamePattern{
		footprint.MustPattern(TypeRole, roles),
		footprint.MustPattern(TypeInstanceProfile, roles),
		footprint.MustPattern(TypeRolePolicy, `q-[0-9a-f]{12}`),
		footprint.MustPattern(TypeQueue, `calque-(?:pool|fleet)-[a-z0-9_-]+`),
		footprint.MustPattern(TypeSecurityGroup, NFSSecurityGroupName),
		footprint.MustPattern(TypeImage, `ami-[0-9a-f]+`),
		footprint.MustPattern(TypeSnapshot, `snap-[0-9a-f]+`),
		footprint.MustPattern(TypeInstance, `i-[0-9a-f]+`),
		footprint.MustPattern(TypeObject, `(?:runs|fleet|ami-bake)/.+`),
	}
}

// matchesName reports whether a resource of type typ called name matches one
// of Patterns.
func matchesName(typ, name string) bool {
	r := footprint.Resource{Type: typ, Name: name}
	for _, p := range Patterns() {
		if p.Matches(r) {
			return true
		}
	}
	return false
}

// PoolQueueName is the SQS queue name of the pool for model.
func PoolQueueName(model string) string { return PoolQueuePrefix + Slug(model) }

// RunQueueName is the SQS queue name of the fleet run runID.
func RunQueueName(runID string) string { return FleetQueuePrefix + Slug(runID) }

// Slug makes s safe as the tail of an SQS queue name: lowercased, every
// character outside [a-z0-9_] turned into '-', runs of '-' collapsed, leading
// and trailing '-' trimmed, and cut so PoolQueuePrefix plus the slug fits SQS's
// 80-character limit.
func Slug(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(lower))
	lastDash := false
	for _, r := range lower {
		var out rune
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out = r
		default:
			out = '-'
		}
		if out == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(out)
	}
	slug := strings.Trim(b.String(), "-")
	maxLen := 80 - len(PoolQueuePrefix)
	if len(slug) > maxLen {
		slug = strings.TrimRight(slug[:maxLen], "-")
	}
	return slug
}

// QueueGrantPolicyName is the name of the inline policy on a worker role that
// grants one queue: "q-" plus the first 12 hex characters of SHA-256(queue
// ARN) (calque#226).
func QueueGrantPolicyName(queueARN string) string {
	h := sha256.Sum256([]byte(queueARN))
	return QueueGrantPrefix + hex.EncodeToString(h[:])[:12]
}

// workerRoleForQueue is the shared role whose workers consume queue name, or
// "" if it is not a calque pool or fleet queue.
func workerRoleForQueue(name string) string {
	switch {
	case strings.HasPrefix(name, PoolQueuePrefix):
		return PoolWorkerRole
	case strings.HasPrefix(name, FleetQueuePrefix):
		return FleetWorkerRole
	}
	return ""
}

// isWorkerRole reports whether role is one of the shared worker roles that
// carry per-queue grants.
func isWorkerRole(role string) bool { return role == PoolWorkerRole || role == FleetWorkerRole }

// isResultKey reports whether an S3 key under calque's prefixes is a run's
// output (its results or summary) rather than calque's staging. Outputs are
// the user's data and are never classed litter.
func isResultKey(key string) bool {
	return strings.Contains(key, "/results/") || strings.HasSuffix(key, "/summary.json")
}
