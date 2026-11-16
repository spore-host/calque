package calquefootprint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/calque/internal/awstest"
)

const (
	ssmPolicy    = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
	sporedPolicy = "arn:aws:iam::" + acct + ":policy/spored-instance-policy"
	realRun      = RealRunRolePrefix + "0123456789abcdef"
	legacyRun    = RealRunRolePrefix + "fedcba9876543210"
	serviceRun   = RealRunRolePrefix + "00000000000000aa"
)

func grantDoc(queueARN string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sqs:ReceiveMessage","sqs:DeleteMessage"],"Resource":[%q]}]}`, queueARN)
}

// seed builds an account holding a busy pool, an idle pool, an idle fleet
// queue, an unused real-run role, a legacy and a service role, two
// calque-nfs-sg groups (one in use, one referenced by an EFS group), a baked
// AMI, and S3 staging and results.
func seed() *awstest.Account {
	a := awstest.NewAccount(acct, "us-east-1", "us-west-2")
	e1 := "us-east-1"
	busy, idle, fleet := PoolQueueName("org/busy"), PoolQueueName("org/idle"), RunQueueName("run1")
	gone := a.QueueARN(e1, PoolQueueName("org/gone"))
	inline := func(grants ...string) map[string]string {
		m := map[string]string{"spored-baseline-policy": `{"Statement":[]}`, "spawn-scoped-policy": `{"Statement":[]}`}
		for _, g := range grants {
			m[QueueGrantPolicyName(g)] = grantDoc(g)
		}
		return m
	}
	a.AddRole(PoolWorkerRole, calqueTags("pool-workers"), old, inline(a.QueueARN(e1, busy), a.QueueARN(e1, idle), gone), ssmPolicy, sporedPolicy)
	a.AddRole(FleetWorkerRole, calqueTags("fleet-workers"), old, inline(a.QueueARN(e1, fleet)), ssmPolicy, sporedPolicy)
	a.AddRole(realRun, calqueTags("bucket:b"), old, inline(), ssmPolicy, sporedPolicy)
	a.AddRole(legacyRun, map[string]string{"spawn:managed": "true"}, old, inline(), ssmPolicy)
	svc := calqueTags("bucket:svc")
	svc[tags.Role] = tags.RoleService
	a.AddRole(serviceRun, svc, old, inline(), ssmPolicy)

	a.AddQueue(e1, busy, calqueTags("pool:org/busy"), old, 0, 0)
	a.AddQueue(e1, idle, calqueTags("pool:org/idle"), old, 0, 0)
	a.AddQueue(e1, fleet, calqueTags("run:run1"), old, 0, 0)
	a.AddInstance(e1, "i-0b05", "running", map[string]string{tags.ManagedBy: "calque", managedTag: "true", poolModelTag: "org/busy"}, a.ProfileARN(PoolWorkerRole))
	a.AddInstance(e1, "i-0dead", "terminated", map[string]string{managedTag: "true"}, a.ProfileARN(realRun))

	a.AddSecurityGroup(e1, "sg-0aaa", NFSSecurityGroupName, "vpc-1", calqueTags("vpc:vpc-1"))
	a.AddENI(e1, "eni-1", "sg-0aaa")
	a.AddSecurityGroup(e1, "sg-0bbb", NFSSecurityGroupName, "vpc-2", calqueTags("vpc:vpc-2"))
	a.AddSecurityGroup(e1, "sg-0ef5", "efs-mount-target", "vpc-2", map[string]string{"owner": "user"})
	a.AddRule(e1, "sg-0ef5", false, "sg-0bbb")

	a.AddImage("us-west-2", "ami-0123", withKV(calqueTags("run:bake"), BakedImageTag, "vllm"), "snap-0123")

	a.AddObject("b", "runs/r1/artifacts/warmd", calqueTags("run:r1"), old)
	a.AddObject("b", "runs/r1/results/0000000001.json", calqueTags("run:r1"), old)
	a.AddObject("b", "fleet/r2/manifest.json", calqueTags("run:r2"), now.Add(-time.Hour))
	a.AddObject("b", "runs/someone-else/notes.txt", nil, old)
	return a
}

func inventory(t *testing.T, a *awstest.Account) (*Footprint, Clients) {
	t.Helper()
	c := ClientsFromConfig(a.Config())
	fp, err := Inventory(context.Background(), c, Options{
		Account: acct, Regions: []string{"us-east-1", "us-west-2"}, AllRegions: true, Buckets: []string{"b"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return fp, c
}

func localLease(t *testing.T) *lifecycle.Lease {
	t.Helper()
	f := lifecycletest.New()
	lc, err := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return f, nil }, ControlRegion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := lc.Observe(ctx, Tool, lifecycle.ScopeGlobal, false); err != nil {
		t.Fatal(err)
	}
	l, err := lc.Claim(ctx, Tool, lifecycle.ScopeGlobal, 0)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func execute(t *testing.T, a *awstest.Account, fp *Footprint, c Clients, mode footprint.Mode) *footprint.Result {
	t.Helper()
	plan, err := footprint.NewPlan(Tool, Patterns(), fp.Report.Resources)
	if err != nil {
		t.Fatal(err)
	}
	var lease *lifecycle.Lease
	if mode == footprint.ModeReap {
		lease = localLease(t)
	}
	d := NewDeleter(c, DeleterOptions{Regions: []string{"us-east-1", "us-west-2"}, Now: func() time.Time { return now }})
	res, err := footprint.Execute(context.Background(), plan, mode, d, lease)
	for _, act := range res.Actions {
		if act.Outcome == footprint.OutcomeFailed && !errors.Is(act.Err, ErrActive) {
			t.Errorf("%s %s failed: %v", act.Type, act.Name, act.Err)
		}
	}
	_ = err // joins the ErrActive refusals, checked per action above
	return res
}

func outcome(t *testing.T, res *footprint.Result, typ, name string) footprint.Action {
	t.Helper()
	for _, a := range res.Actions {
		if a.Type == typ && a.Name == name {
			return a
		}
	}
	t.Fatalf("no action for %s %s", typ, name)
	return footprint.Action{}
}

func TestInventoryFromAccount(t *testing.T) {
	fp, _ := inventory(t, seed())
	wantClass(t, find(t, fp, TypeQueue, PoolQueueName("org/busy")), footprint.ClassActive, true)
	wantClass(t, find(t, fp, TypeQueue, PoolQueueName("org/idle")), footprint.ClassLitter, true)
	wantClass(t, find(t, fp, TypeRole, PoolWorkerRole), footprint.ClassActive, true)
	wantClass(t, find(t, fp, TypeRole, FleetWorkerRole), footprint.ClassLitter, true)
	wantClass(t, find(t, fp, TypeRole, realRun), footprint.ClassLitter, true)
	wantClass(t, find(t, fp, TypeImage, "ami-0123"), footprint.ClassUserData, false)
	wantClass(t, find(t, fp, TypeObject, "runs/r1/artifacts/warmd"), footprint.ClassLitter, true)
	if r := find(t, fp, TypeRole, legacyRun); r.Ownership != footprint.OwnershipLegacyName || r.Deletable {
		t.Errorf("legacy role: %s deletable %v", r.Ownership, r.Deletable)
	}
	if r := find(t, fp, TypeRole, serviceRun); r.Deletable {
		t.Errorf("role=service role is deletable")
	}
	grants := 0
	for _, r := range fp.Report.Resources {
		if r.Type == TypeRolePolicy {
			grants++
		}
		if r.Type == TypeInstance && r.Name == "i-0dead" {
			t.Error("a terminated instance is in the footprint")
		}
	}
	if grants != 4 {
		t.Errorf("%d queue grants, want 4 (spawn's inline policies are not grants)", grants)
	}
	q := fp.Details[find(t, fp, TypeQueue, PoolQueueName("org/busy")).ARN].Queue
	if q == nil || len(q.Workers) != 1 {
		t.Errorf("busy queue detail %+v", q)
	}
	if d := fp.Details[find(t, fp, TypeRole, realRun).ARN]; !slices.Contains(d.ManagedPolicies, sporedPolicy) {
		t.Errorf("real-run role detail lists managed policies %v", d.ManagedPolicies)
	}
	if !strings.Contains(strings.Join(fp.Report.Warnings, "\n"), "1 object(s)") {
		t.Errorf("no warning about the untagged object: %v", fp.Report.Warnings)
	}
}

func TestReportModeDeletesNothing(t *testing.T) {
	a := seed()
	fp, c := inventory(t, a)
	res := execute(t, a, fp, c, footprint.ModeReport)
	if ops := a.Ops(); len(ops) > 0 {
		t.Fatalf("report mode changed the account: %v", ops)
	}
	if got := outcome(t, res, TypeQueue, PoolQueueName("org/idle")).Outcome; got != footprint.OutcomeWouldDelete {
		t.Errorf("idle queue: %s", got)
	}
	if got := outcome(t, res, TypeRole, realRun).Outcome; got != footprint.OutcomeWouldDelete {
		t.Errorf("real-run role: %s", got)
	}
	busy := outcome(t, res, TypeQueue, PoolQueueName("org/busy"))
	if busy.Outcome != footprint.OutcomeFailed || !errors.Is(busy.Err, ErrActive) {
		t.Errorf("busy queue: %s %v", busy.Outcome, busy.Err)
	}
}

func TestReapInDependencyOrder(t *testing.T) {
	a := seed()
	fp, c := inventory(t, a)
	res := execute(t, a, fp, c, footprint.ModeReap)
	ops := a.Ops()
	idx := func(op string) int {
		t.Helper()
		i := slices.Index(ops, op)
		if i < 0 {
			t.Fatalf("op %q not done; ops:\n%s", op, strings.Join(ops, "\n"))
		}
		return i
	}
	before := func(x, y string) {
		t.Helper()
		if idx(x) > idx(y) {
			t.Errorf("%q came after %q", x, y)
		}
	}
	idleGrant := QueueGrantPolicyName(a.QueueARN("us-east-1", PoolQueueName("org/idle")))
	fleetGrant := QueueGrantPolicyName(a.QueueARN("us-east-1", RunQueueName("run1")))
	before("SQS:DeleteQueue "+PoolQueueName("org/idle"), "IAM:DeleteRolePolicy "+PoolWorkerRole+"/"+idleGrant)
	before("SQS:DeleteQueue "+RunQueueName("run1"), "IAM:DeleteRolePolicy "+FleetWorkerRole+"/"+fleetGrant)

	// Role teardown: out of the profile, profile deleted, inline policies
	// deleted, managed policies detached, then the role.
	for _, role := range []string{FleetWorkerRole, realRun} {
		before("IAM:RemoveRoleFromInstanceProfile "+role+" "+role, "IAM:DeleteInstanceProfile "+role)
		before("IAM:DeleteInstanceProfile "+role, "IAM:DeleteRole "+role)
		before("IAM:DeleteRolePolicy "+role+"/spored-baseline-policy", "IAM:DetachRolePolicy "+role+" "+ssmPolicy)
		before("IAM:DeleteRolePolicy "+role+"/spawn-scoped-policy", "IAM:DeleteRole "+role)
		before("IAM:DetachRolePolicy "+role+" "+sporedPolicy, "IAM:DeleteRole "+role)
		if a.HasRole(role) || a.HasProfile(role) {
			t.Errorf("%s or its profile survived", role)
		}
	}
	for _, op := range ops {
		if strings.Contains(op, "UNMODELLED") || strings.Contains(op, "DeletePolicy ") {
			t.Errorf("unexpected call %q: spawn's managed policies are only detached", op)
		}
	}

	// The busy pool and the role its worker uses are untouched; the idle
	// pool's and the gone queue's grants went.
	if !a.HasQueue("us-east-1", PoolQueueName("org/busy")) || !a.HasRole(PoolWorkerRole) || !a.HasProfile(PoolWorkerRole) {
		t.Error("the busy pool's queue, role or profile was deleted")
	}
	busyGrant := QueueGrantPolicyName(a.QueueARN("us-east-1", PoolQueueName("org/busy")))
	if got, want := a.InlinePolicies(PoolWorkerRole), []string{busyGrant, "spawn-scoped-policy", "spored-baseline-policy"}; !slices.Equal(got, want) {
		t.Errorf("calque-pool-worker keeps %v, want %v", got, want)
	}
	if got := outcome(t, res, TypeRolePolicy, busyGrant); got.Outcome != footprint.OutcomeSkipped && !errors.Is(got.Err, ErrActive) {
		t.Errorf("busy grant: %s", got.Outcome)
	}

	// The legacy and service roles, and their profiles, are kept by the
	// guard (a role behind a kept profile is skipped).
	for _, role := range []string{legacyRun, serviceRun} {
		if got := outcome(t, res, TypeInstanceProfile, role).Outcome; got != footprint.OutcomeKept || !a.HasProfile(role) {
			t.Errorf("profile %s: %s", role, got)
		}
		if got := outcome(t, res, TypeRole, role).Outcome; (got != footprint.OutcomeKept && got != footprint.OutcomeSkipped) || !a.HasRole(role) {
			t.Errorf("role %s: %s", role, got)
		}
	}

	// The unused SG: the EFS group's reference and its own rule revoked
	// first, then deleted. The EFS group itself stays.
	before("EC2:RevokeSecurityGroupIngress sg-0ef5 sg-0bbb", "EC2:DeleteSecurityGroup sg-0bbb")
	before("EC2:RevokeSecurityGroupIngress sg-0bbb sg-0bbb", "EC2:DeleteSecurityGroup sg-0bbb")
	if !a.HasGroup("us-east-1", "sg-0ef5") || !slices.Equal(a.Rules("us-east-1", "sg-0ef5"), []string{"ingress:sg-0ef5"}) || a.HasGroup("us-east-1", "sg-0bbb") {
		t.Errorf("security groups after reap: efs %v rules %v, bbb %v", a.HasGroup("us-east-1", "sg-0ef5"), a.Rules("us-east-1", "sg-0ef5"), a.HasGroup("us-east-1", "sg-0bbb"))
	}
	if !a.HasGroup("us-east-1", "sg-0aaa") {
		t.Error("the SG with an ENI was deleted")
	}

	// AMIs, results and fresh staging are kept; old staging is gone.
	if got := outcome(t, res, TypeImage, "ami-0123").Outcome; got != footprint.OutcomeKept {
		t.Errorf("AMI: %s", got)
	}
	if a.HasObject("b", "runs/r1/artifacts/warmd") || !a.HasObject("b", "runs/r1/results/0000000001.json") || !a.HasObject("b", "fleet/r2/manifest.json") {
		t.Error("wrong S3 objects deleted")
	}
	if !a.HasObject("b", "runs/someone-else/notes.txt") {
		t.Error("an untagged object under runs/ was deleted")
	}
}

func TestReapRechecksLiveState(t *testing.T) {
	a := seed()
	fp, c := inventory(t, a)
	// After the plan: someone queues work on the idle pool, and the real-run
	// role is re-tagged as the live service's.
	a.SetQueueBacklog("us-east-1", PoolQueueName("org/idle"), 5)
	svc := calqueTags("bucket:b")
	svc[tags.Role] = tags.RoleService
	a.SetRoleTags(realRun, svc)
	res := execute(t, a, fp, c, footprint.ModeReap)

	q := outcome(t, res, TypeQueue, PoolQueueName("org/idle"))
	if !errors.Is(q.Err, ErrActive) || !a.HasQueue("us-east-1", PoolQueueName("org/idle")) {
		t.Errorf("busy-again queue: %s %v", q.Outcome, q.Err)
	}
	g := outcome(t, res, TypeRolePolicy, QueueGrantPolicyName(a.QueueARN("us-east-1", PoolQueueName("org/idle"))))
	if g.Outcome != footprint.OutcomeSkipped {
		t.Errorf("its grant: %s, want skipped", g.Outcome)
	}
	r := outcome(t, res, TypeRole, realRun)
	if r.Outcome != footprint.OutcomeRefused || !a.HasRole(realRun) {
		t.Errorf("re-tagged role=service: %s (%s)", r.Outcome, r.Reason)
	}
}
