package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskcohort"
)

// TestWorkerPolicyScopesToThisPoolOnly proves the IAM policy is scoped to the
// SPECIFIC model's queue and the given manifest/results buckets — never a
// wildcard across all pools or all buckets (least-privilege, mirroring
// spawn's own poolWorkerPolicy discipline).
func TestWorkerPolicyScopesToThisPoolOnly(t *testing.T) {
	policy := WorkerPolicy("111122223333", "us-east-1", "resnet", "calque-manifests", "calque-results")

	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("WorkerPolicy did not produce valid JSON: %v\n%s", err, policy)
	}

	wantQueueARN := "arn:aws:sqs:us-east-1:111122223333:calque-pool-resnet"
	if !strings.Contains(policy, wantQueueARN) {
		t.Errorf("policy missing scoped queue ARN %q; got %s", wantQueueARN, policy)
	}
	// Must NOT grant SendMessage/CreateQueue/DeleteQueue — those are the
	// submitter's operations, not the worker's (mirrors spawn's split).
	for _, forbidden := range []string{"sqs:SendMessage", "sqs:CreateQueue", "sqs:DeleteQueue"} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("policy grants %q, which is a submitter-only action a worker must not have", forbidden)
		}
	}
	if !strings.Contains(policy, "calque-manifests") {
		t.Errorf("policy missing manifest bucket scoping; got %s", policy)
	}
	if !strings.Contains(policy, "calque-results") {
		t.Errorf("policy missing results bucket scoping; got %s", policy)
	}
	// A DIFFERENT model's queue must not appear anywhere in this policy.
	if strings.Contains(policy, "calque-pool-bert") {
		t.Errorf("policy for model %q leaked a reference to a different model's queue", "resnet")
	}
}

// TestBuildWorkerBootstrapCommandIsShellSafe proves the bootstrap command
// quotes every argument, so a model/path containing a space (a plausible HF
// repo-style name is unlikely to, but a runner path under a directory with a
// space is not implausible) doesn't silently truncate the command.
func TestBuildWorkerBootstrapCommandIsShellSafe(t *testing.T) {
	cmd := buildWorkerBootstrapCommand("resnet-50", "us-east-1", "/opt/calque runner/runner.py", "30m", 900)
	for _, want := range []string{
		`--model "resnet-50"`,
		`--region "us-east-1"`,
		`--runner-path "/opt/calque runner/runner.py"`,
		`--idle-timeout "30m"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("bootstrap command missing %q; got: %s", want, cmd)
		}
	}
}

// ---- fakes for DeletePool/PoolStatus (calque#130) --------------------------
//
// fakeLaunchAPI is an in-memory taskcohort.LaunchAPI — the same seam
// ProvisionWorkers's Actuator/Observer already run behind (see
// spawn's pkg/taskcohort/adapter_test.go's fakeLauncher, mirrored here since
// calque cannot import spawn's internal test type). It records instances by
// Name so ListInstances/Terminate can be driven deterministically without
// real AWS.
type fakeLaunchAPI struct {
	mu         sync.Mutex
	instances  map[string]spawnaws.InstanceInfo // name -> instance
	terminated []string                         // instance IDs Terminate was called for
}

func newFakeLaunchAPI() *fakeLaunchAPI {
	return &fakeLaunchAPI{instances: map[string]spawnaws.InstanceInfo{}}
}

// seed adds an instance tagged as belonging to model, as ProvisionWorkers's
// own base.Tags would have stamped it.
func (f *fakeLaunchAPI) seed(name, instanceID, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[name] = spawnaws.InstanceInfo{
		InstanceID: instanceID,
		Name:       name,
		State:      "running",
		Tags:       map[string]string{poolModelTag: model},
	}
}

// seedFleet is seed's calque#145 sibling: tags the instance with
// fleetWorkerTag (calque:fleet-run) instead of poolModelTag, as
// ProvisionFleetWorkers' own base.Tags would have stamped it.
func (f *fakeLaunchAPI) seedFleet(name, instanceID, runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[name] = spawnaws.InstanceInfo{
		InstanceID: instanceID,
		Name:       name,
		State:      "running",
		Tags:       map[string]string{fleetWorkerTag: runID},
	}
}

func (f *fakeLaunchAPI) Launch(_ context.Context, _ spawnaws.LaunchConfig) (*spawnaws.LaunchResult, error) {
	return nil, fmt.Errorf("fakeLaunchAPI: Launch not needed by DeletePool/PoolStatus tests")
}

func (f *fakeLaunchAPI) Terminate(_ context.Context, _, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, instanceID)
	for name, in := range f.instances {
		if in.InstanceID == instanceID {
			delete(f.instances, name)
		}
	}
	return nil
}

func (f *fakeLaunchAPI) StartInstance(_ context.Context, _, _ string) error {
	return fmt.Errorf("fakeLaunchAPI: StartInstance not needed by DeletePool/PoolStatus tests")
}

func (f *fakeLaunchAPI) ListInstances(_ context.Context, _, _ string) ([]spawnaws.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]spawnaws.InstanceInfo, 0, len(f.instances))
	for _, in := range f.instances {
		out = append(out, in)
	}
	return out, nil
}

func (f *fakeLaunchAPI) terminatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.terminated)
}

// fakeSQS is an in-memory taskpool.SQSAPI covering exactly the operations
// DeletePoolQueueIfExists/SQSQueue.Depth/DeleteQueue need — mirrors spawn's
// own pkg/taskpool/worker_test.go fakeSQS shape, trimmed to what's exercised
// here. queues maps QueueName -> live/deleted + depth counters.
type fakeSQS struct {
	mu     sync.Mutex
	queues map[string]*fakeQueueState
}

type fakeQueueState struct {
	url        string
	deleted    bool
	visible    int
	notVisible int
}

func newFakeSQS() *fakeSQS {
	return &fakeSQS{queues: map[string]*fakeQueueState{}}
}

// seedQueue creates a live queue for name with the given depth counters, as
// if CreatePoolQueue had already run and some claims were in flight.
func (f *fakeSQS) seedQueue(name string, visible, notVisible int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[name] = &fakeQueueState{url: "mem://" + name, visible: visible, notVisible: notVisible}
}

func (f *fakeSQS) CreateQueue(_ context.Context, in *sqs.CreateQueueInput, _ ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	name := aws.ToString(in.QueueName)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[name] = &fakeQueueState{url: "mem://" + name}
	return &sqs.CreateQueueOutput{QueueUrl: aws.String("mem://" + name)}, nil
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	name := aws.ToString(in.QueueName)
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.queues[name]
	if !ok || q.deleted {
		return nil, &types.QueueDoesNotExist{}
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String(q.url)}, nil
}

func (f *fakeSQS) SendMessage(_ context.Context, _ *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return nil, fmt.Errorf("fakeSQS: SendMessage not needed by DeletePool/PoolStatus tests")
}

// ChangeMessageVisibility is a stub satisfying the pool.SQSAPI interface
// (calque#131's heartbeat extension) — not exercised by DeletePool/PoolStatus
// tests, which never claim/heartbeat a message.
func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, _ *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return nil, fmt.Errorf("fakeSQS: ChangeMessageVisibility not needed by DeletePool/PoolStatus tests")
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return nil, fmt.Errorf("fakeSQS: ReceiveMessage not needed by DeletePool/PoolStatus tests")
}

func (f *fakeSQS) DeleteMessage(_ context.Context, _ *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return nil, fmt.Errorf("fakeSQS: DeleteMessage not needed by DeletePool/PoolStatus tests")
}

func (f *fakeSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	url := aws.ToString(in.QueueUrl)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queues {
		if q.url == url && !q.deleted {
			return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
				string(types.QueueAttributeNameApproximateNumberOfMessages):           fmt.Sprintf("%d", q.visible),
				string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible): fmt.Sprintf("%d", q.notVisible),
			}}, nil
		}
	}
	return nil, fmt.Errorf("fakeSQS: no live queue at %s", url)
}

func (f *fakeSQS) DeleteQueue(_ context.Context, in *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	url := aws.ToString(in.QueueUrl)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queues {
		if q.url == url {
			q.deleted = true
			return &sqs.DeleteQueueOutput{}, nil
		}
	}
	return nil, fmt.Errorf("fakeSQS: no queue at %s to delete", url)
}

func (f *fakeSQS) queueDeleted(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.queues[name]
	return ok && q.deleted
}

// ---- DeletePool -------------------------------------------------------------

// TestDeletePool_TerminatesOnlyThisModelsWorkers proves DeletePool discovers
// workers by the calque:pool-model tag ProvisionWorkers stamps, terminates
// every one of THIS pool's workers, and leaves a different pool's workers
// (same account, different model tag) completely untouched — deleting one
// pool must never touch another pool's billable instances.
func TestDeletePool_TerminatesOnlyThisModelsWorkers(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seed("calque-pool-resnet-worker-0", "i-1", "resnet")
	launch.seed("calque-pool-resnet-worker-1", "i-2", "resnet")
	launch.seed("calque-pool-bert-worker-0", "i-3", "bert") // different pool

	sqsc := newFakeSQS()
	sqsc.seedQueue(PoolQueueName("resnet"), 2, 1)
	sqsc.seedQueue(PoolQueueName("bert"), 0, 0)

	if err := deletePool(context.Background(), launch, sqsc, "resnet", "us-east-1"); err != nil {
		t.Fatalf("deletePool: %v", err)
	}

	if launch.terminatedCount() != 2 {
		t.Errorf("terminated count = %d, want 2 (resnet's two workers)", launch.terminatedCount())
	}
	remaining, err := launch.ListInstances(context.Background(), "us-east-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Name != "calque-pool-bert-worker-0" {
		t.Errorf("expected only bert's worker to survive, got %+v", remaining)
	}
	if !sqsc.queueDeleted(PoolQueueName("resnet")) {
		t.Error("resnet's queue was not deleted")
	}
	if sqsc.queueDeleted(PoolQueueName("bert")) {
		t.Error("bert's queue was deleted, but DeletePool was only asked to delete resnet's pool")
	}
}

// TestDeletePool_NoWorkersStillDeletesQueue: a pool that was scaled down to
// zero workers (or never finished provisioning) but still has a live queue
// must still have that queue deleted — "zero workers" and "the queue exists"
// are independent facts, and DeletePool's job is to remove BOTH regardless of
// which one is currently non-empty.
func TestDeletePool_NoWorkersStillDeletesQueue(t *testing.T) {
	launch := newFakeLaunchAPI() // no workers seeded
	sqsc := newFakeSQS()
	sqsc.seedQueue(PoolQueueName("resnet"), 0, 0)

	if err := deletePool(context.Background(), launch, sqsc, "resnet", "us-east-1"); err != nil {
		t.Fatalf("deletePool: %v", err)
	}
	if !sqsc.queueDeleted(PoolQueueName("resnet")) {
		t.Error("queue was not deleted even though the pool had zero workers")
	}
}

// TestDeletePool_NoQueueIsNotAnError: deleting a pool whose queue was already
// removed (a second `calque pool delete`, or a pool that never got past
// worker provisioning) must succeed, not fail — deletion is idempotent.
func TestDeletePool_NoQueueIsNotAnError(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seed("calque-pool-resnet-worker-0", "i-1", "resnet")
	sqsc := newFakeSQS() // no queue seeded at all

	if err := deletePool(context.Background(), launch, sqsc, "resnet", "us-east-1"); err != nil {
		t.Fatalf("deletePool with no pre-existing queue should succeed, got: %v", err)
	}
	if launch.terminatedCount() != 1 {
		t.Errorf("terminated count = %d, want 1", launch.terminatedCount())
	}
}

// ---- PoolStatus -------------------------------------------------------------

// TestPoolStatus_CountsRunningWorkersAndQueueDepth proves PoolStatus reports
// the worker count as observed StateRunning instances (not merely "listed")
// and the queue depth as visible+in-flight — the two figures calque#130
// scoped status/list down to.
func TestPoolStatus_CountsRunningWorkersAndQueueDepth(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seed("calque-pool-resnet-worker-0", "i-1", "resnet")
	launch.seed("calque-pool-resnet-worker-1", "i-2", "resnet")
	launch.seed("calque-pool-bert-worker-0", "i-3", "bert") // must not be counted

	sqsc := newFakeSQS()
	sqsc.seedQueue(PoolQueueName("resnet"), 3, 2)

	status, err := poolStatus(context.Background(), launch, sqsc, "resnet", "us-east-1")
	if err != nil {
		t.Fatalf("poolStatus: %v", err)
	}
	if status.Model != "resnet" {
		t.Errorf("Model = %q, want %q", status.Model, "resnet")
	}
	if status.WorkerCount != 2 {
		t.Errorf("WorkerCount = %d, want 2", status.WorkerCount)
	}
	if !status.QueueExists {
		t.Error("QueueExists = false, want true")
	}
	if status.QueueDepth != 5 {
		t.Errorf("QueueDepth = %d, want 5 (3 visible + 2 in-flight)", status.QueueDepth)
	}
}

// TestPoolStatus_NoQueueReportsExistsFalse: a pool whose queue was deleted (or
// never created) must report QueueExists=false with zero depth, not an
// error — a status call must be safe to run against a torn-down or
// never-fully-created pool.
func TestPoolStatus_NoQueueReportsExistsFalse(t *testing.T) {
	launch := newFakeLaunchAPI()
	sqsc := newFakeSQS() // no queue

	status, err := poolStatus(context.Background(), launch, sqsc, "resnet", "us-east-1")
	if err != nil {
		t.Fatalf("poolStatus: %v", err)
	}
	if status.QueueExists {
		t.Error("QueueExists = true, want false (no queue was ever created)")
	}
	if status.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0", status.QueueDepth)
	}
	if status.WorkerCount != 0 {
		t.Errorf("WorkerCount = %d, want 0", status.WorkerCount)
	}
}

// TestPoolStatus_NonRunningWorkerNotCounted: a worker observed in a
// non-Running state (e.g. still launching) must not count toward
// WorkerCount — WorkerCount answers "how many workers can actually claim
// work right now," not "how many instances exist."
func TestPoolStatus_NonRunningWorkerNotCounted(t *testing.T) {
	launch := newFakeLaunchAPI()
	launch.seed("calque-pool-resnet-worker-0", "i-1", "resnet")
	launch.mu.Lock()
	in := launch.instances["calque-pool-resnet-worker-0"]
	in.State = "pending"
	launch.instances["calque-pool-resnet-worker-0"] = in
	launch.mu.Unlock()

	sqsc := newFakeSQS()

	status, err := poolStatus(context.Background(), launch, sqsc, "resnet", "us-east-1")
	if err != nil {
		t.Fatalf("poolStatus: %v", err)
	}
	if status.WorkerCount != 0 {
		t.Errorf("WorkerCount = %d, want 0 (worker is still pending, not running)", status.WorkerCount)
	}
}

// fakeScaleLaunchAPI is an in-memory taskcohort.LaunchAPI — no real AWS. It mirrors
// spawn's own pkg/taskcohort/adapter_test.go fakeLauncher (same seam,
// deliberately the same shape) so ScaleWorkers' reconcile choreography can be
// driven deterministically for calque#115's tests: pre-seed some "already
// running" workers, then observe that a scale-up only ever touches new ones.
type fakeScaleLaunchAPI struct {
	mu         sync.Mutex
	launched   map[string]spawnaws.InstanceInfo // name -> instance
	nextID     int
	relaunched map[string]int // name -> how many times Launch was called for it
}

func newFakeScaleLaunchAPI() *fakeScaleLaunchAPI {
	return &fakeScaleLaunchAPI{
		launched:   map[string]spawnaws.InstanceInfo{},
		relaunched: map[string]int{},
	}
}

// seed pre-populates n already-running workers named calque-pool-<model>-worker-0..n-1,
// simulating a pool ProvisionWorkers already brought up.
func (f *fakeScaleLaunchAPI) seed(model string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.nextID++
		name := fmt.Sprintf("calque-pool-%s-worker-%d", model, i)
		f.launched[name] = spawnaws.InstanceInfo{
			InstanceID: fmt.Sprintf("i-seed-%d", f.nextID),
			Name:       name,
			State:      "running",
			PrivateIP:  fmt.Sprintf("10.0.0.%d", f.nextID),
		}
	}
}

func (f *fakeScaleLaunchAPI) Launch(_ context.Context, cfg spawnaws.LaunchConfig) (*spawnaws.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relaunched[cfg.Name]++
	f.nextID++
	inst := spawnaws.InstanceInfo{
		InstanceID: fmt.Sprintf("i-%d", f.nextID),
		Name:       cfg.Name,
		State:      "running",
		PrivateIP:  fmt.Sprintf("10.0.1.%d", f.nextID),
	}
	f.launched[cfg.Name] = inst
	return &spawnaws.LaunchResult{
		InstanceID: inst.InstanceID,
		Name:       cfg.Name,
		PrivateIP:  inst.PrivateIP,
		State:      "running",
	}, nil
}

func (f *fakeScaleLaunchAPI) Terminate(_ context.Context, _, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, in := range f.launched {
		if in.InstanceID == instanceID {
			delete(f.launched, name)
		}
	}
	return nil
}

func (f *fakeScaleLaunchAPI) StartInstance(_ context.Context, _, _ string) error { return nil }

func (f *fakeScaleLaunchAPI) ListInstances(_ context.Context, _, _ string) ([]spawnaws.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]spawnaws.InstanceInfo, 0, len(f.launched))
	for _, in := range f.launched {
		out = append(out, in)
	}
	return out, nil
}

// launchCountFor returns how many times Launch was called for the given
// worker name (0 if never).
func (f *fakeScaleLaunchAPI) launchCountFor(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.relaunched[name]
}

func (f *fakeScaleLaunchAPI) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launched)
}

// TestCurrentWorkerCount_ProbesUntilFirstUnknown proves currentWorkerCount
// stops exactly at the first StateUnknown gap in the deterministic
// calque-pool-<model>-worker-<i> naming — the only "how many members does this
// cohort have" signal cohort.Observer exposes (there is no bulk membership
// query anywhere in cohort's or taskcohort's API).
func TestCurrentWorkerCount_ProbesUntilFirstUnknown(t *testing.T) {
	f := newFakeScaleLaunchAPI()
	f.seed("resnet", 3) // worker-0, worker-1, worker-2 already exist

	obs := &taskcohort.Observer{Client: f, Region: "us-east-1"}
	got, err := currentWorkerCount(context.Background(), obs, "resnet")
	if err != nil {
		t.Fatalf("currentWorkerCount: %v", err)
	}
	if got != 3 {
		t.Fatalf("currentWorkerCount = %d, want 3", got)
	}
}

// TestCurrentWorkerCount_ZeroWhenNoWorkersExist proves a never-scaled pool
// (or a pool name typo) reports 0, not an error — the natural base case for
// ScaleWorkers being called against a freshly-created, still-empty pool.
func TestCurrentWorkerCount_ZeroWhenNoWorkersExist(t *testing.T) {
	f := newFakeScaleLaunchAPI()
	obs := &taskcohort.Observer{Client: f, Region: "us-east-1"}
	got, err := currentWorkerCount(context.Background(), obs, "resnet")
	if err != nil {
		t.Fatalf("currentWorkerCount: %v", err)
	}
	if got != 0 {
		t.Fatalf("currentWorkerCount = %d, want 0", got)
	}
}

// TestScaleWorkersCore_NumbersFromObservedCount is the load-bearing test for
// calque#115: with 3 workers (worker-0..2) already live, scaling by 2 must
// create worker-3 and worker-4 — NOT restart numbering at worker-0, which
// would collide with (and, per cohort.Reconciler's unconditional per-member
// Launch, re-provision) the already-running workers.
func TestScaleWorkersCore_NumbersFromObservedCount(t *testing.T) {
	f := newFakeScaleLaunchAPI()
	f.seed("resnet", 3)

	act := &taskcohort.Actuator{
		Client:     f,
		Region:     "us-east-1",
		BaseConfig: spawnaws.LaunchConfig{InstanceType: "g5.xlarge", Region: "us-east-1"},
	}
	obs := &taskcohort.Observer{Client: f, Region: "us-east-1"}

	if err := scaleWorkersCore(context.Background(), act, obs, "resnet", "g5.xlarge", false, 2); err != nil {
		t.Fatalf("scaleWorkersCore: %v", err)
	}

	// worker-0, worker-1, worker-2 (pre-existing) must NEVER have had Launch
	// called on them by this scale-up — only worker-3 and worker-4 (the new,
	// delta-only members) should show a Launch call.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("calque-pool-resnet-worker-%d", i)
		if got := f.launchCountFor(name); got != 0 {
			t.Errorf("pre-existing %s was Launched %d time(s) by ScaleWorkers; want 0 (must not disturb existing members)", name, got)
		}
	}
	for i := 3; i < 5; i++ {
		name := fmt.Sprintf("calque-pool-resnet-worker-%d", i)
		if got := f.launchCountFor(name); got != 1 {
			t.Errorf("new %s was Launched %d time(s); want exactly 1", name, got)
		}
	}

	// All 5 workers (3 pre-existing + 2 new) should now be live.
	if got := f.liveCount(); got != 5 {
		t.Fatalf("live workers after scale-up = %d, want 5 (3 pre-existing + 2 new)", got)
	}
}

// TestScaleWorkersCore_RejectsNonPositiveAddWorkers guards the same
// "addWorkers must be >= 1" contract ScaleWorkers documents.
func TestScaleWorkersCore_RejectsNonPositiveAddWorkers(t *testing.T) {
	if err := ScaleWorkers(context.Background(), nil, CreateConfig{Model: "resnet", InstanceType: "g5.xlarge"}, 0); err == nil {
		t.Fatal("ScaleWorkers with addWorkers=0 should error, got nil")
	}
	if err := ScaleWorkers(context.Background(), nil, CreateConfig{Model: "resnet", InstanceType: "g5.xlarge"}, -1); err == nil {
		t.Fatal("ScaleWorkers with addWorkers=-1 should error, got nil")
	}
}
