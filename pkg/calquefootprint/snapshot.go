package calquefootprint

import "time"

// Snapshot is what Collect read from an account: the raw facts, before
// Classify judges them. Collect fills it from AWS; tests and the reaper can
// build one directly.
type Snapshot struct {
	Account string
	// Partition is the ARN partition ("aws" if empty).
	Partition string
	// Regions are the regions whose regional resources (queues, security
	// groups, AMIs, instances) were listed.
	Regions []string
	// AllRegions says Regions is every region enabled in the account. Only
	// then can a global resource (an IAM role) be judged unused: an instance
	// in an unlisted region could be using it.
	AllRegions bool

	Roles          []Role
	Profiles       []Profile
	Queues         []Queue
	SecurityGroups []SecurityGroup
	Images         []Image
	Snapshots      []EBSSnapshot
	Instances      []Instance
	Objects        []Object

	// Incomplete names the listings that failed, by key: "iam", or
	// "<service>:<region>" ("ec2:us-east-1", "sqs:us-east-1",
	// "eni:us-east-1"), or "s3:<bucket>". Classify treats what it cannot see
	// as activity, so a failed listing never turns something into litter.
	Incomplete map[string]bool
	// Warnings are reported verbatim in the Report.
	Warnings []string
}

// Role is an IAM role whose name matches calque's role names.
type Role struct {
	Name, ARN string
	Tags      map[string]string
	Created   time.Time
	// InlinePolicies are the names of its inline policies.
	InlinePolicies []string
	// Grants are its per-queue inline policies (q-*), with the queue each
	// grants.
	Grants []Grant
	// ManagedPolicies are the ARNs of the managed policies attached to it.
	// Teardown detaches them and never deletes them.
	ManagedPolicies []string
}

// Grant is one per-queue inline policy (q-*) on a worker role.
type Grant struct {
	Policy string
	// QueueARN is the queue the policy grants, read from its document; ""
	// if the document could not be read or names no single queue.
	QueueARN string
}

// Profile is an IAM instance profile whose name matches calque's role names.
type Profile struct {
	Name, ARN string
	Tags      map[string]string
	Created   time.Time
	// Roles are the names of the roles in it.
	Roles []string
}

// Queue is an SQS queue named calque-pool-* or calque-fleet-*.
type Queue struct {
	Region, Name, URL, ARN string
	Tags                   map[string]string
	Created                time.Time
	// Visible, InFlight and Delayed are the queue's approximate message
	// counts.
	Visible, InFlight, Delayed int
}

// SecurityGroup is a calque-nfs-sg group.
type SecurityGroup struct {
	Region, ID, Name, VPC string
	Tags                  map[string]string
	// ENIs are the network interfaces using the group.
	ENIs []string
}

// Image is an AMI tagged calque:baked-image.
type Image struct {
	Region, ID, Name, State string
	Tags                    map[string]string
	// Snapshots are its EBS snapshots' IDs.
	Snapshots []string
}

// EBSSnapshot is an EBS snapshot tagged calque:baked-image.
type EBSSnapshot struct {
	Region, ID string
	Tags       map[string]string
}

// Instance is an EC2 instance tagged calque:managed=true that is not
// terminated.
type Instance struct {
	Region, ID, State string
	Tags              map[string]string
	// ProfileARN is its instance profile's ARN, if it has one.
	ProfileARN string
}

// Object is an S3 object under one of S3Prefixes in a bucket passed to
// Collect, tagged spore-host:managed-by=calque.
type Object struct {
	Bucket, Region, Key string
	Tags                map[string]string
	LastModified        time.Time
	Size                int64
}
