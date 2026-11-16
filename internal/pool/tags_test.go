package pool

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/internal/calquetags"
	calexec "github.com/spore-host/calque/internal/exec"
	warm "github.com/spore-host/calque/worker/warm-runner"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// ---- calque#227: everything the pool package creates is tagged ---------------

// sqsService answers CreateQueue/ListQueueTags/TagQueue for one queue,
// recording the CreateQueue input; the queue's tags are whatever create set.
type sqsService struct {
	mu     sync.Mutex
	create *sqs.CreateQueueInput
	tags   map[string]string
	tagged []map[string]string // TagQueue calls
	// createErrs are returned by successive CreateQueue calls before one
	// succeeds.
	createErrs []error
}

func (s *sqsService) handle(op string, in any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch v := in.(type) {
	case *sqs.CreateQueueInput:
		if len(s.createErrs) > 0 {
			err := s.createErrs[0]
			s.createErrs = s.createErrs[1:]
			return nil, err
		}
		s.create = v
		if s.tags == nil {
			s.tags = v.Tags
		}
		return &sqs.CreateQueueOutput{QueueUrl: awssdk.String("https://sqs.test/" + awssdk.ToString(v.QueueName))}, nil
	case *sqs.ListQueueTagsInput:
		return &sqs.ListQueueTagsOutput{Tags: s.tags}, nil
	case *sqs.TagQueueInput:
		s.tagged = append(s.tagged, v.Tags)
		return &sqs.TagQueueOutput{}, nil
	}
	return nil, awstest.Unhandled("SQS", op)
}

func captureTagWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := calquetags.Warn
	calquetags.Warn = &buf
	t.Cleanup(func() { calquetags.Warn = prev })
	return &buf
}

// TestCreatePoolQueue_TaggedAtCreate: the pool queue's CreateQueue carries
// managed-by=calque (tagtest), scope pool:<model> and calque:pool-model, and
// states its message retention instead of relying on the SQS default.
func TestCreatePoolQueue_TaggedAtCreate(t *testing.T) {
	svc := &sqsService{}
	cfg := awstest.Config(awstest.Services{"SQS": svc.handle})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	if _, err := CreatePoolQueue(context.Background(), sqs.NewFromConfig(cfg), "llama", 900); err != nil {
		t.Fatal(err)
	}
	got := svc.create.Tags
	for k, v := range map[string]string{tags.Scope: "pool:llama", calquetags.PoolModel: "llama", calquetags.Managed: "true"} {
		if got[k] != v {
			t.Errorf("Tags[%q] = %q, want %q", k, got[k], v)
		}
	}
	if r := svc.create.Attributes["MessageRetentionPeriod"]; r != "345600" {
		t.Errorf("MessageRetentionPeriod = %q, want 345600 (SQS's default, which existing pool queues have)", r)
	}
	if len(svc.tagged) != 0 {
		t.Errorf("TagQueue called %v on a queue tagged at create", svc.tagged)
	}
}

// TestCreateRunQueue_TaggedAtCreate: the fleet run queue likewise.
func TestCreateRunQueue_TaggedAtCreate(t *testing.T) {
	svc := &sqsService{}
	cfg := awstest.Config(awstest.Services{"SQS": svc.handle})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)

	if _, err := CreateRunQueue(context.Background(), sqs.NewFromConfig(cfg), "run-9", 900); err != nil {
		t.Fatal(err)
	}
	got := svc.create.Tags
	for k, v := range map[string]string{tags.Scope: "run:run-9", calquetags.FleetRun: "run-9", calquetags.RunID: "run-9"} {
		if got[k] != v {
			t.Errorf("Tags[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestCreatePoolQueue_ConvergesAnUntaggedExistingQueue: CreateQueue returns
// an existing queue as is, so a pool queue an older calque created stays
// untagged unless calque reads its tags back and adds the missing ones.
func TestCreatePoolQueue_ConvergesAnUntaggedExistingQueue(t *testing.T) {
	f := newFakeSQS()
	f.seedQueue(PoolQueueName("llama"), 0, 0) // untagged, as before calque#227
	if _, err := CreatePoolQueue(context.Background(), f, "llama", 900); err != nil {
		t.Fatal(err)
	}
	got := f.tagsOf(PoolQueueName("llama"))
	if !tags.IsManagedBy(got, tags.ToolCalque) || got[calquetags.PoolModel] != "llama" {
		t.Errorf("existing queue's tags = %v, want calque's set added by TagQueue", got)
	}
}

// TestCreateQueue_AccessDeniedCreatesUntagged: without sqs:TagQueue a tagged
// CreateQueue is refused. The queue is then created untagged with a
// warning, rather than failing the pool (spawn's taskpool does the same).
func TestCreateQueue_AccessDeniedCreatesUntagged(t *testing.T) {
	warn := captureTagWarn(t)
	svc := &sqsService{createErrs: []error{&awstest.APIError{Code: "AccessDenied", Message: "sqs:TagQueue"}}}
	cfg := awstest.Config(awstest.Services{"SQS": svc.handle})
	tagtest.New(t, tags.ToolCalque, tagtest.Allow{
		Service: "SQS", Operation: "CreateQueue",
		Reason: "this test's second CreateQueue is the deliberate untagged fallback for a caller without sqs:TagQueue",
	}).Install(&cfg)

	if _, err := CreatePoolQueue(context.Background(), sqs.NewFromConfig(cfg), "llama", 900); err != nil {
		t.Fatal(err)
	}
	if svc.create == nil || svc.create.Tags != nil {
		t.Errorf("fallback CreateQueue tags = %v, want none", svc.create)
	}
	if !strings.Contains(warn.String(), "sqs:TagQueue") {
		t.Errorf("warning = %q, want it to name sqs:TagQueue", warn.String())
	}
}

// TestCreateQueue_AlreadyExistsRetriesUntagged: if SQS ever counted tags
// toward CreateQueue's must-match rule, an existing queue's tagged create
// would fail QueueAlreadyExists; the untagged retry then gets the queue and
// TagQueue tags it.
func TestCreateQueue_AlreadyExistsRetriesUntagged(t *testing.T) {
	svc := &sqsService{createErrs: []error{&sqstypes.QueueNameExists{Message: awssdk.String("tags differ")}}, tags: map[string]string{}}
	cfg := awstest.Config(awstest.Services{"SQS": svc.handle})
	if _, err := CreatePoolQueue(context.Background(), sqs.NewFromConfig(cfg), "llama", 900); err != nil {
		t.Fatal(err)
	}
	if len(svc.tagged) != 1 || !tags.IsManagedBy(svc.tagged[0], tags.ToolCalque) {
		t.Errorf("TagQueue calls = %v, want one adding calque's set", svc.tagged)
	}
}

var t0Pool = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

// iamConfig is an awstest IAM plus STS, with tagtest installed, for driving
// spawn's CreateOrGetInstanceProfile through real SDK clients.
func iamConfig(t *testing.T, f *awstest.IAM) (awssdk.Config, *tagtest.Detector) {
	t.Helper()
	cfg := awstest.Config(awstest.Services{"IAM": f.Handle, "STS": awstest.STS(testAccount)})
	d := tagtest.New(t, tags.ToolCalque)
	d.Install(&cfg)
	return cfg, d
}

// TestEnsureWorkerProfile_SpawnCreatesTheRoleTagged drives the real spawn
// v0.131.0 CreateOrGetInstanceProfile: its CreateRole and
// CreateInstanceProfile must carry managed-by=calque (tagtest fails the test
// otherwise), and the role ends up with both calque's and spawn's tags.
func TestEnsureWorkerProfile_SpawnCreatesTheRoleTagged(t *testing.T) {
	f := awstest.NewIAM()
	cfg, d := iamConfig(t, f)
	g := poolWorkerGrant(testAccount, poolCfg("llama"))
	if _, err := ensureWorkerProfile(context.Background(), spawnaws.NewClientFromConfig(cfg), iam.NewFromConfig(cfg), g); err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, c := range d.Calls() {
		if c.Tagged {
			created = append(created, c.Operation)
		}
	}
	if strings.Join(created, ",") != "CreateRole,CreateInstanceProfile" {
		t.Errorf("tagged creates = %v, want CreateRole and CreateInstanceProfile", created)
	}
	for _, got := range []map[string]string{f.RoleTags(PoolWorkerRoleName), f.ProfileTags(PoolWorkerRoleName)} {
		if !tags.IsManagedBy(got, tags.ToolCalque) || got[tags.Scope] != calquetags.ScopePoolWorkers || got["spawn:managed"] != "true" {
			t.Errorf("tags = %v, want calque's set (scope %s) beside spawn's", got, calquetags.ScopePoolWorkers)
		}
	}
}

// TestEnsureWorkerProfile_TagsAnOlderRole: a calque-fleet-worker role an
// older calque created has only spawn's tags; spawn reuses it without
// tagging, so calque converges it.
func TestEnsureWorkerProfile_TagsAnOlderRole(t *testing.T) {
	f := awstest.NewIAM()
	f.SeedRole(FleetWorkerRoleName, map[string]string{"spawn:managed": "true"})
	cfg, _ := iamConfig(t, f)
	g := fleetWorkerGrant(testAccount, FleetCreateConfig{RunID: "r1", Region: "us-east-1", ManifestBucket: "m", ResultsBucket: "r"})
	if _, err := ensureWorkerProfile(context.Background(), spawnaws.NewClientFromConfig(cfg), iam.NewFromConfig(cfg), g); err != nil {
		t.Fatal(err)
	}
	for _, got := range []map[string]string{f.RoleTags(FleetWorkerRoleName), f.ProfileTags(FleetWorkerRoleName)} {
		if !tags.IsManagedBy(got, tags.ToolCalque) || got[tags.Scope] != calquetags.ScopeFleetWorkers {
			t.Errorf("tags = %v, want calque's set (scope %s)", got, calquetags.ScopeFleetWorkers)
		}
	}
}

// TestWorkerPolicies_GrantPutObjectTagging: S3 refuses a tagged PutObject
// without s3:PutObjectTagging, so the workers' results grant includes it.
func TestWorkerPolicies_GrantPutObjectTagging(t *testing.T) {
	for name, doc := range map[string]string{
		"pool":  WorkerPolicy(testAccount, "us-east-1", "m", "mb", "rb"),
		"fleet": FleetWorkerPolicy(testAccount, "us-east-1", "r1", "mb", "rb"),
	} {
		if !strings.Contains(doc, `"s3:GetObject","s3:PutObject","s3:PutObjectTagging"],"Resource":["arn:aws:s3:::rb/*"]`) {
			t.Errorf("%s policy lacks s3:PutObjectTagging on the results bucket: %s", name, doc)
		}
	}
}

// TestWorkerInstanceTags: pool and fleet worker instances are calque's
// (managed-by=calque), with their pool or run as scope, and keep the calque:*
// keys pool status and fleet drain look them up by.
func TestWorkerInstanceTags(t *testing.T) {
	p := poolWorkerTags("llama", t0Pool)
	if !tags.IsManagedBy(p, tags.ToolCalque) || p[tags.Scope] != "pool:llama" || p[poolModelTag] != "llama" || p[calquetags.Managed] != "true" {
		t.Errorf("pool worker tags = %v", p)
	}
	f := fleetWorkerTags("run#1", t0Pool)
	if !tags.IsManagedBy(f, tags.ToolCalque) || f[fleetWorkerTag] != "run#1" || f[calquetags.RunID] != "run#1" {
		t.Errorf("fleet worker tags = %v, want the run id verbatim under %s and %s", f, fleetWorkerTag, calquetags.RunID)
	}
	for k := range p {
		if strings.HasPrefix(k, "spawn:") {
			t.Errorf("pool worker tags carry spawn-reserved key %q", k)
		}
	}
}

// TestS3Results_TagsWhatAWorkerWrites: a pool or fleet worker's results and
// summary are tagged with the manifest's run id.
func TestS3Results_TagsWhatAWorkerWrites(t *testing.T) {
	var mu sync.Mutex
	var taggings []string
	cfg := awstest.Config(awstest.Services{"S3": func(op string, in any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		taggings = append(taggings, awssdk.ToString(in.(*s3.PutObjectInput).Tagging))
		return &s3.PutObjectOutput{}, nil
	}})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)
	r := &S3Results{Client: s3.NewFromConfig(cfg)}
	man := calexec.Manifest{RunID: "run-3", Bucket: "b", ResultPrefix: "runs/run-3/results", SummaryKey: "runs/run-3/summary.json"}
	ctx := context.Background()
	if err := r.Sink(man).Put(ctx, warm.Result{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteSummary(ctx, man, nil, true, 0, calexec.OccupancyRaw{}); err != nil {
		t.Fatal(err)
	}
	if len(taggings) != 2 {
		t.Fatalf("PutObject calls = %d, want 2", len(taggings))
	}
	for _, tg := range taggings {
		if !strings.Contains(tg, "calque%3Arun-id=run-3") {
			t.Errorf("Tagging = %q, want calque:run-id=run-3", tg)
		}
	}
}
