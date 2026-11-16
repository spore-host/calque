package calquefootprint

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
)

const acct = "111122223333"

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// old is well past the grace.
var old = now.Add(-72 * time.Hour)

func calqueTags(scope string) map[string]string {
	m := tags.For(Tool, scope, old, "")
	m[managedTag] = "true"
	return m
}

func roleARN(name string) string    { return "arn:aws:iam::" + acct + ":role/" + name }
func profileARN(name string) string { return "arn:aws:iam::" + acct + ":instance-profile/" + name }
func queueARN(region, name string) string {
	return "arn:aws:sqs:" + region + ":" + acct + ":" + name
}

// fixture is an account with:
//   - pool "busy" (queue with a live worker) and pool "idle" (no workers, no
//     messages), both granted on calque-pool-worker;
//   - a grant on calque-pool-worker whose queue no longer exists;
//   - a real-run role nothing uses;
//   - calque-nfs-sg in two VPCs, one with an ENI;
//   - a baked AMI and its snapshot;
//   - S3 staging (old and new) and results.
func fixture() *Snapshot {
	busyQ, idleQ := PoolQueueName("org/busy"), PoolQueueName("org/idle")
	busyARN, idleARN := queueARN("us-east-1", busyQ), queueARN("us-east-1", idleQ)
	goneARN := queueARN("us-east-1", PoolQueueName("org/gone"))
	realRun := RealRunRolePrefix + "0123456789abcdef"
	worker := map[string]string{tags.ManagedBy: "calque", poolModelTag: "org/busy", managedTag: "true"}
	return &Snapshot{
		Account: acct, Regions: []string{"us-east-1"}, AllRegions: true, Incomplete: map[string]bool{},
		Roles: []Role{
			{Name: PoolWorkerRole, ARN: roleARN(PoolWorkerRole), Tags: calqueTags("pool-workers"), Created: old,
				InlinePolicies: []string{QueueGrantPolicyName(busyARN), QueueGrantPolicyName(idleARN), QueueGrantPolicyName(goneARN), "spored-baseline-policy"},
				Grants: []Grant{
					{Policy: QueueGrantPolicyName(busyARN), QueueARN: busyARN},
					{Policy: QueueGrantPolicyName(idleARN), QueueARN: idleARN},
					{Policy: QueueGrantPolicyName(goneARN), QueueARN: goneARN},
				},
				ManagedPolicies: []string{"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}},
			{Name: realRun, ARN: roleARN(realRun), Tags: calqueTags("bucket:b"), Created: old},
		},
		Profiles: []Profile{
			{Name: PoolWorkerRole, ARN: profileARN(PoolWorkerRole), Tags: calqueTags("pool-workers"), Created: old, Roles: []string{PoolWorkerRole}},
			{Name: realRun, ARN: profileARN(realRun), Tags: calqueTags("bucket:b"), Created: old, Roles: []string{realRun}},
		},
		Queues: []Queue{
			{Region: "us-east-1", Name: busyQ, ARN: busyARN, Tags: calqueTags("pool:org/busy"), Created: old},
			{Region: "us-east-1", Name: idleQ, ARN: idleARN, Tags: calqueTags("pool:org/idle"), Created: old},
		},
		SecurityGroups: []SecurityGroup{
			{Region: "us-east-1", ID: "sg-0aaa", Name: NFSSecurityGroupName, VPC: "vpc-1", Tags: calqueTags("vpc:vpc-1"), ENIs: []string{"eni-1"}},
			{Region: "us-east-1", ID: "sg-0bbb", Name: NFSSecurityGroupName, VPC: "vpc-2", Tags: calqueTags("vpc:vpc-2")},
		},
		Images:    []Image{{Region: "us-east-1", ID: "ami-0123", Tags: withKV(calqueTags("run:bake"), BakedImageTag, "vllm"), Snapshots: []string{"snap-0123"}}},
		Snapshots: []EBSSnapshot{{Region: "us-east-1", ID: "snap-0123", Tags: withKV(calqueTags("run:bake"), BakedImageTag, "vllm")}},
		Instances: []Instance{{Region: "us-east-1", ID: "i-0b05", State: "running", Tags: worker, ProfileARN: profileARN(PoolWorkerRole)}},
		Objects: []Object{
			{Bucket: "b", Key: "runs/r1/artifacts/warmd", Tags: calqueTags("run:r1"), LastModified: old},
			{Bucket: "b", Key: "runs/r1/results/0000000001.json", Tags: calqueTags("run:r1"), LastModified: old},
			{Bucket: "b", Key: "runs/r1/summary.json", Tags: calqueTags("run:r1"), LastModified: old},
			{Bucket: "b", Key: "fleet/r2/manifest.json", Tags: calqueTags("run:r2"), LastModified: now.Add(-time.Hour)},
		},
	}
}

func withKV(m map[string]string, k, v string) map[string]string {
	m[k] = v
	return m
}

func find(t *testing.T, fp *Footprint, typ, name string) footprint.Resource {
	t.Helper()
	for _, r := range fp.Report.Resources {
		if r.Type == typ && r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s %q in the footprint", typ, name)
	return footprint.Resource{}
}

func wantClass(t *testing.T, r footprint.Resource, class footprint.Class, deletable bool) {
	t.Helper()
	if r.Class != class || r.Deletable != deletable {
		t.Errorf("%s %s: class %s deletable %v (%s); want %s deletable %v", r.Type, r.Name, r.Class, r.Deletable, r.Reason, class, deletable)
	}
}

func TestClassifyActivePool(t *testing.T) {
	s := fixture()
	fp := Classify(s, now, 0)
	busy := find(t, fp, TypeQueue, PoolQueueName("org/busy"))
	wantClass(t, busy, footprint.ClassActive, true)
	if d := fp.Details[busy.ARN]; d.Queue == nil || !reflect.DeepEqual(d.Queue.Workers, []string{"i-0b05"}) {
		t.Errorf("busy queue detail = %+v, want worker i-0b05", fp.Details[busy.ARN].Queue)
	}
	grant := find(t, fp, TypeRolePolicy, QueueGrantPolicyName(busy.ARN))
	wantClass(t, grant, footprint.ClassActive, true)
	if !reflect.DeepEqual(grant.DependsOn, []string{roleARN(PoolWorkerRole)}) {
		t.Errorf("grant depends on %v, want its role", grant.DependsOn)
	}
	if want := []string{grant.ARN, roleARN(PoolWorkerRole)}; !reflect.DeepEqual(busy.DependsOn, want) {
		t.Errorf("queue depends on %v, want %v", busy.DependsOn, want)
	}
}

func TestClassifyIdleQueueIsLitter(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	idle := find(t, fp, TypeQueue, PoolQueueName("org/idle"))
	wantClass(t, idle, footprint.ClassLitter, true)
	wantClass(t, find(t, fp, TypeRolePolicy, QueueGrantPolicyName(idle.ARN)), footprint.ClassLitter, true)
	gone := QueueGrantPolicyName(queueARN("us-east-1", PoolQueueName("org/gone")))
	wantClass(t, find(t, fp, TypeRolePolicy, gone), footprint.ClassLitter, true)
}

func TestClassifyMessagesKeepQueueActive(t *testing.T) {
	s := fixture()
	s.Queues[1].Visible = 3
	fp := Classify(s, now, 0)
	wantClass(t, find(t, fp, TypeQueue, PoolQueueName("org/idle")), footprint.ClassActive, true)
}

func TestClassifyRoleUsedByLiveInstance(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	role := find(t, fp, TypeRole, PoolWorkerRole)
	wantClass(t, role, footprint.ClassActive, true)
	if got := fp.Details[role.ARN].UsedBy; !reflect.DeepEqual(got, []string{"i-0b05"}) {
		t.Errorf("used by %v", got)
	}
	wantClass(t, find(t, fp, TypeInstanceProfile, PoolWorkerRole), footprint.ClassActive, true)
	wantClass(t, find(t, fp, TypeInstance, "i-0b05"), footprint.ClassActive, true)
	// The unused real-run role is litter, and its profile goes before it.
	rr := RealRunRolePrefix + "0123456789abcdef"
	wantClass(t, find(t, fp, TypeRole, rr), footprint.ClassLitter, true)
	prof := find(t, fp, TypeInstanceProfile, rr)
	wantClass(t, prof, footprint.ClassLitter, true)
	if !reflect.DeepEqual(prof.DependsOn, []string{roleARN(rr)}) {
		t.Errorf("profile depends on %v", prof.DependsOn)
	}
}

func TestClassifyRolesNeedEveryRegion(t *testing.T) {
	s := fixture()
	s.AllRegions = false
	fp := Classify(s, now, 0)
	rr := RealRunRolePrefix + "0123456789abcdef"
	wantClass(t, find(t, fp, TypeRole, rr), footprint.ClassActive, true)
	wantClass(t, find(t, fp, TypeInstanceProfile, rr), footprint.ClassActive, true)
	if idle, why := fp.Idle(); idle || why == "" {
		t.Errorf("Idle() = %v %q with one region listed", idle, why)
	}
}

func TestClassifyFailedListingIsActive(t *testing.T) {
	s := fixture()
	s.Instances = nil
	s.Incomplete["ec2:us-east-1"] = true
	fp := Classify(s, now, 0)
	wantClass(t, find(t, fp, TypeQueue, PoolQueueName("org/idle")), footprint.ClassActive, true)
	wantClass(t, find(t, fp, TypeRole, RealRunRolePrefix+"0123456789abcdef"), footprint.ClassActive, true)
	if !strings.Contains(strings.Join(fp.Report.Warnings, "\n"), "ec2:us-east-1") {
		t.Errorf("warnings %v do not name the failed listing", fp.Report.Warnings)
	}
}

func TestClassifySecurityGroups(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	var withENI, without footprint.Resource
	for _, r := range fp.Report.Resources {
		if r.Type == TypeSecurityGroup && strings.HasSuffix(r.ARN, "sg-0aaa") {
			withENI = r
		}
		if r.Type == TypeSecurityGroup && strings.HasSuffix(r.ARN, "sg-0bbb") {
			without = r
		}
	}
	wantClass(t, withENI, footprint.ClassActive, true)
	if got := fp.Details[withENI.ARN].ENIs; !reflect.DeepEqual(got, []string{"eni-1"}) {
		t.Errorf("ENIs %v", got)
	}
	wantClass(t, without, footprint.ClassLitter, true)
}

func TestClassifyAMIsAreUserData(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	ami := find(t, fp, TypeImage, "ami-0123")
	wantClass(t, ami, footprint.ClassUserData, false)
	wantClass(t, find(t, fp, TypeSnapshot, "snap-0123"), footprint.ClassUserData, false)
	if !strings.Contains(ami.Reason, "never deleted") {
		t.Errorf("AMI reason %q", ami.Reason)
	}
}

func TestClassifyObjects(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	wantClass(t, find(t, fp, TypeObject, "runs/r1/artifacts/warmd"), footprint.ClassLitter, true)
	wantClass(t, find(t, fp, TypeObject, "runs/r1/results/0000000001.json"), footprint.ClassUserData, false)
	wantClass(t, find(t, fp, TypeObject, "runs/r1/summary.json"), footprint.ClassUserData, false)
	wantClass(t, find(t, fp, TypeObject, "fleet/r2/manifest.json"), footprint.ClassActive, true)
}

func TestClassifyGrace(t *testing.T) {
	s := fixture()
	s.Queues[1].Tags = calqueTags("pool:org/idle")
	s.Queues[1].Tags[tags.Created] = tags.FormatCreated(now.Add(-time.Hour))
	fp := Classify(s, now, 0)
	wantClass(t, find(t, fp, TypeQueue, PoolQueueName("org/idle")), footprint.ClassActive, true)
}

func TestClassifyLegacyNameNotDeletable(t *testing.T) {
	s := fixture()
	rr := RealRunRolePrefix + "0123456789abcdef"
	s.Roles[1].Tags = map[string]string{"spawn:managed": "true"} // made before calque#227
	s.Profiles[1].Tags = nil
	s.Queues[1].Tags = nil
	fp := Classify(s, now, 0)
	for _, r := range []footprint.Resource{find(t, fp, TypeRole, rr), find(t, fp, TypeInstanceProfile, rr), find(t, fp, TypeQueue, PoolQueueName("org/idle"))} {
		if r.Ownership != footprint.OwnershipLegacyName || r.Deletable {
			t.Errorf("%s %s: ownership %s deletable %v; want legacy-name, not deletable", r.Type, r.Name, r.Ownership, r.Deletable)
		}
		if r.Class != footprint.ClassLitter {
			t.Errorf("%s %s: class %s; a legacy resource is still classified", r.Type, r.Name, r.Class)
		}
	}
}

func TestClassifyGuardRefusesServiceAndForeign(t *testing.T) {
	s := fixture()
	rr := RealRunRolePrefix + "0123456789abcdef"
	s.Roles[1].Tags[tags.Role] = tags.RoleService
	s.Queues[1].Tags[tags.ManagedBy] = string(tags.ToolSpawn)
	fp := Classify(s, now, 0)
	role := find(t, fp, TypeRole, rr)
	if role.Deletable || !strings.Contains(role.Reason, "service") {
		t.Errorf("role=service role: deletable %v, reason %q", role.Deletable, role.Reason)
	}
	q := find(t, fp, TypeQueue, PoolQueueName("org/idle"))
	if q.Ownership != footprint.OwnershipForeign || q.Deletable {
		t.Errorf("spawn-managed queue: ownership %s deletable %v", q.Ownership, q.Deletable)
	}
}

func TestFootprintJSONRoundTrip(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	b, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	// The shared decoder reads it as a plain Report.
	rep, err := footprint.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("footprint.Decode: %v", err)
	}
	if !sameJSON(t, rep.Resources, fp.Report.Resources) || rep.Account != acct || rep.Tool != Tool {
		t.Errorf("footprint.Decode lost data")
	}
	var back Footprint
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Details, fp.Details) {
		t.Errorf("details lost in the round trip")
	}
	if !sameJSON(t, back.Report.Resources, fp.Report.Resources) || !back.Report.GeneratedAt.Equal(fp.Report.GeneratedAt) {
		t.Errorf("report lost in the round trip")
	}
	// Empty collections are arrays and objects, never null.
	empty, _ := json.Marshal(Classify(&Snapshot{Account: acct, AllRegions: true}, now, 0))
	for _, want := range []string{`"resources":[]`, `"details":{}`} {
		if !bytes.Contains(empty, []byte(want)) {
			t.Errorf("empty footprint %s lacks %s", empty, want)
		}
	}
}

// sameJSON compares two values by their JSON, which is what a consumer sees
// (an empty and a nil depends_on are both []).
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ja, jb)
}

func TestPlanOrder(t *testing.T) {
	fp := Classify(fixture(), now, 0)
	plan, err := footprint.NewPlan(Tool, Patterns(), fp.Report.Resources)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, st := range plan.Steps {
		pos[st.Resource.ARN] = i
	}
	idle := queueARN("us-east-1", PoolQueueName("org/idle"))
	grant := InlinePolicyID(roleARN(PoolWorkerRole), QueueGrantPolicyName(idle))
	rr := RealRunRolePrefix + "0123456789abcdef"
	for _, pair := range [][2]string{
		{idle, grant}, {grant, roleARN(PoolWorkerRole)}, {profileARN(PoolWorkerRole), roleARN(PoolWorkerRole)},
		{profileARN(rr), roleARN(rr)}, {"arn:aws:ec2:us-east-1:" + acct + ":instance/i-0b05", profileARN(PoolWorkerRole)},
	} {
		if pos[pair[0]] >= pos[pair[1]] {
			t.Errorf("%s (step %d) must come before %s (step %d)", pair[0], pos[pair[0]], pair[1], pos[pair[1]])
		}
	}
}

func TestNamesAgree(t *testing.T) {
	if got := PoolQueueName("Qwen/Qwen2.5-1.5B-Instruct"); got != "calque-pool-qwen-qwen2-5-1-5b-instruct" {
		t.Errorf("PoolQueueName = %q", got)
	}
	if !matchesName(TypeQueue, PoolQueueName("Qwen/Qwen2.5")) || !matchesName(TypeQueue, RunQueueName("2026-11-16-a")) {
		t.Error("queue names do not match calque's pattern")
	}
	if !matchesName(TypeRolePolicy, QueueGrantPolicyName("arn:aws:sqs:us-east-1:1:q")) {
		t.Error("grant name does not match calque's pattern")
	}
	if matchesName(TypeRolePolicy, "spored-baseline-policy") || matchesName(TypeRole, "spored-instance-role") {
		t.Error("spawn's names match calque's patterns")
	}
	role, policy, ok := splitInlinePolicyID(InlinePolicyID(roleARN(PoolWorkerRole), "q-0123456789ab"))
	if !ok || role != PoolWorkerRole || policy != "q-0123456789ab" {
		t.Errorf("splitInlinePolicyID = %q %q %v", role, policy, ok)
	}
}
