// Package calquetags is the one place calque builds the tags it puts on the
// AWS resources it creates (calque#227), so a later footprint/teardown
// (calque#228) can find exactly what calque made and nothing else.
//
// Every resource gets the shared spore-host:* set from
// github.com/spore-host/libs/tags with spore-host:managed-by=calque, plus
// calque:managed=true, the key calque's own tag filters already use. Callers
// add their resource-specific calque:* keys (calque:run-id, calque:pool-model,
// ...) on top.
//
// Instances calque launches through spawn carry spawn's own spawn:* tags too.
// spore-host:managed-by on them is calque, not spawn: calque terminates them
// (deferred terminate, fleet drain, pool delete), so calque is the tool
// responsible for removing them. spawn's TTL backstop keys on its spawn:*
// lifecycle tags and is unaffected.
package calquetags

import (
	"strings"
	"time"
	"unicode"

	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/s3tags"
)

// calque's own tag keys.
const (
	// Managed marks every resource calque creates. Its value is "true".
	Managed = "calque:managed"
	// RunID names the run a run-scoped resource belongs to.
	RunID = "calque:run-id"
	// PoolModel names the model pool a pool-scoped resource belongs to.
	PoolModel = "calque:pool-model"
	// FleetRun names the fleet run a fleet queue or worker belongs to.
	FleetRun = "calque:fleet-run"
)

// Scope values (the spore-host:scope tag). The value is opaque to libs/tags;
// these are calque's own conventions.
const (
	// ScopePoolWorkers is the scope of the shared calque-pool-worker role:
	// every pool's workers run under it.
	ScopePoolWorkers = "pool-workers"
	// ScopeFleetWorkers is the scope of the shared calque-fleet-worker role.
	ScopeFleetWorkers = "fleet-workers"
)

// RunScope is the scope of a resource that belongs to one run.
func RunScope(runID string) string { return prefixed("run:", runID) }

// PoolScope is the scope of a resource that belongs to one model pool.
func PoolScope(model string) string { return prefixed("pool:", model) }

// BucketScope is the scope of a resource shared by every run against one
// staging bucket (the per-bucket calque-real-run-<hash> role).
func BucketScope(bucket string) string { return prefixed("bucket:", bucket) }

// VPCScope is the scope of a resource shared by every run in one VPC (the
// calque-nfs-sg security group).
func VPCScope(vpcID string) string { return prefixed("vpc:", vpcID) }

func prefixed(p, v string) string {
	if v == "" {
		return ""
	}
	return p + v
}

// For returns the tag set for a resource calque creates now: the spore-host:*
// set from libs/tags.For (managed-by=calque, scope, created, schema) plus
// calque:managed=true. An empty scope is left out. The result is a new map the
// caller owns.
func For(scope string, now time.Time) map[string]string {
	m := tags.For(tags.ToolCalque, Value(scope), now, "")
	m[Managed] = "true"
	return m
}

// ForRun is For(RunScope(runID), now) plus calque:run-id when runID is set.
func ForRun(runID string, now time.Time) map[string]string {
	m := For(RunScope(runID), now)
	if runID != "" {
		m[RunID] = Value(runID)
	}
	return m
}

// ObjectTagging is the PutObject Tagging query string for an S3 object calque
// writes for runID: managed-by=calque, the schema version, calque:managed,
// and, when runID is set, spore-host:scope and calque:run-id. It has no
// spore-host:created: an object's LastModified already says when it was
// written, and S3 allows only 10 tags per object.
func ObjectTagging(runID string) string {
	m := map[string]string{
		tags.ManagedBy: string(tags.ToolCalque),
		tags.Schema:    tags.SchemaVersion,
		Managed:        "true",
	}
	if runID != "" {
		m[tags.Scope] = Value(RunScope(runID))
		m[RunID] = Value(runID)
	}
	return s3tags.Query(m)
}

// maxValueLen is the tag-value length limit IAM, SQS, S3 and EC2 share.
const maxValueLen = 256

// Value makes v a legal tag value for every service calque tags. IAM, SQS
// and S3 accept only letters, numbers, spaces and _ . : / = + - @ in a tag
// value, at most 256 characters; anything else becomes '_'. A run id, model
// name or image ref is user input, and an illegal character would otherwise
// fail the create call outright.
func Value(v string) string {
	var b strings.Builder
	n := 0
	for _, r := range v {
		if n == maxValueLen {
			break
		}
		if !legalValueRune(r) {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func legalValueRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Zs, r) {
		return true
	}
	return strings.ContainsRune("_.:/=+-@", r)
}

// Converge returns the tags in want that have is missing or holds a
// different value for, which a caller then applies with the service's tag
// call (TagRole, TagQueue, CreateTags). It is how calque brings a resource
// created by an older calque, or re-tagged by something else, up to date
// without rewriting what is already right.
//
// spore-host:created is never moved once present: cleanup's grace period
// counts from it, so re-stamping it on every reuse would keep a resource
// looking new forever. A resource that has none gets want's value, i.e. "now"
// for a resource calque finds but did not tag when it created it.
func Converge(have, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		cur, ok := have[k]
		if k == tags.Created && ok {
			continue
		}
		if !ok || cur != v {
			out[k] = v
		}
	}
	return out
}
