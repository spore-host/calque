package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/pkg/calquefootprint"
)

const fpAcct = "111122223333"

var fpNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func fpTags(scope string) map[string]string {
	m := tags.For(tags.ToolCalque, scope, fpNow.Add(-72*time.Hour), "")
	m["calque:managed"] = "true"
	return m
}

// fpAccount holds an idle pool queue, an unused real-run role and an old
// staging object: all litter.
func fpAccount() *awstest.Account {
	a := awstest.NewAccount(fpAcct, "us-east-1")
	old := fpNow.Add(-72 * time.Hour)
	a.AddRole(calquefootprint.RealRunRolePrefix+"0123456789abcdef", fpTags("bucket:b"), old,
		map[string]string{"spored-baseline-policy": "{}"}, "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore")
	a.AddQueue("us-east-1", calquefootprint.PoolQueueName("m"), fpTags("pool:m"), old, 0, 0)
	a.AddObject("b", "runs/r1/manifest.json", fpTags("run:r1"), old)
	return a
}

func TestParseFootprintArgs(t *testing.T) {
	if _, err := parseFootprintArgs([]string{"--region", "us-west-2", "--all-regions"}); err == nil {
		t.Error("--region with --all-regions accepted")
	}
	if _, err := parseFootprintArgs([]string{"-o", "yaml"}); err == nil {
		t.Error("-o yaml accepted")
	}
	o, err := parseFootprintArgs([]string{"--all-regions", "--bucket", "a,b", "--bucket", "c", "-o", "json"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.allRegions || strings.Join(o.buckets, " ") != "a b c" || o.output != "json" {
		t.Errorf("parsed %+v", o)
	}
}

func TestParseTeardownArgs(t *testing.T) {
	if _, err := parseTeardownArgs(nil); err == nil {
		t.Error("teardown without --plan accepted")
	}
	if _, err := parseTeardownArgs([]string{"--plan", "-", "--mode", "nuke"}); err == nil {
		t.Error("--mode nuke accepted")
	}
	o, err := parseTeardownArgs([]string{"--plan", "-"})
	if err != nil || o.mode != footprint.ModeReport {
		t.Errorf("default mode %q (%v), want report", o.mode, err)
	}
}

func TestNoLeaseFlagIsHidden(t *testing.T) {
	var buf bytes.Buffer
	fsArgs := []string{"-h"}
	// -h prints usage to stderr; capture it by swapping os.Stderr.
	r, w, _ := os.Pipe()
	orig := os.Stderr
	os.Stderr = w
	_, _ = parseTeardownArgs(fsArgs)
	_ = w.Close()
	os.Stderr = orig
	_, _ = buf.ReadFrom(r)
	if strings.Contains(buf.String(), noLeaseFlag) || !strings.Contains(buf.String(), "-yes") {
		t.Errorf("usage:\n%s", buf.String())
	}
}

func footprintJSON(t *testing.T, a *awstest.Account) []byte {
	t.Helper()
	var out bytes.Buffer
	o := footprintOpts{region: "us-east-1", allRegions: true, buckets: []string{"b"}, output: "json"}
	if err := runFootprint(context.Background(), a.Config(), o, &out, fpNow); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestFootprintJSONIsAReport(t *testing.T) {
	b := footprintJSON(t, fpAccount())
	rep, err := footprint.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("footprint -o json is not a footprint.Report: %v\n%s", err, b)
	}
	if rep.Account != fpAcct || rep.Tool != tags.ToolCalque || len(rep.Resources) != 4 {
		t.Errorf("report: account %s tool %s, %d resources", rep.Account, rep.Tool, len(rep.Resources))
	}
	var withDetails map[string]json.RawMessage
	if err := json.Unmarshal(b, &withDetails); err != nil || withDetails["details"] == nil {
		t.Errorf("no details object: %v", err)
	}
}

func TestFootprintTable(t *testing.T) {
	var out bytes.Buffer
	o := footprintOpts{region: "us-east-1", output: "table"}
	if err := runFootprint(context.Background(), fpAccount().Config(), o, &out, fpNow); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SQS::Queue", "messages 0 visible", "workers 0", "--all-regions"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}

type teardownRun struct {
	acct   *awstest.Account
	dynamo *lifecycletest.Fake
	out    bytes.Buffer
	errOut bytes.Buffer
}

func (tr *teardownRun) run(t *testing.T, plan []byte, args ...string) error {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, plan, 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := parseTeardownArgs(append([]string{"--plan", path}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return runTeardown(context.Background(), o, teardownEnv{
		cfg:    tr.acct.Config(),
		dynamo: func(string) (lifecycle.API, error) { return tr.dynamo, nil },
		now:    func() time.Time { return fpNow },
		out:    &tr.out, errOut: &tr.errOut,
	})
}

func newTeardownRun() *teardownRun {
	return &teardownRun{acct: fpAccount(), dynamo: lifecycletest.New()}
}

func TestTeardownReportMakesNoDeletes(t *testing.T) {
	tr := newTeardownRun()
	if err := tr.run(t, footprintJSON(t, tr.acct)); err != nil {
		t.Fatal(err)
	}
	if ops := tr.acct.Ops(); len(ops) > 0 {
		t.Errorf("report mode changed the account: %v", ops)
	}
	if !strings.Contains(tr.out.String(), "would-delete 4") {
		t.Errorf("output:\n%s", tr.out.String())
	}
	if tr.dynamo.Calls("UpdateItem")+tr.dynamo.Calls("CreateTable") > 0 {
		t.Error("report mode touched the lease table")
	}
}

func TestTeardownReapNeedsYes(t *testing.T) {
	tr := newTeardownRun()
	err := tr.run(t, footprintJSON(t, tr.acct), "--mode", "reap")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("reap without --yes: %v", err)
	}
	if len(tr.acct.Ops()) > 0 {
		t.Error("deleted without --yes")
	}
}

func TestTeardownReapRefusesWithoutLease(t *testing.T) {
	tr := newTeardownRun()
	err := tr.run(t, footprintJSON(t, tr.acct), "--mode", "reap", "--yes")
	if err == nil || !strings.Contains(err.Error(), "lease") {
		t.Errorf("reap without a lease: %v", err)
	}
	if len(tr.acct.Ops()) > 0 {
		t.Error("deleted without a lease")
	}
	if tr.dynamo.Calls("CreateTable") > 0 {
		t.Error("looking for the lease created the lifecycle table")
	}
}

func TestTeardownReapWithoutLeaseWhenTold(t *testing.T) {
	tr := newTeardownRun()
	if err := tr.run(t, footprintJSON(t, tr.acct), "--mode", "reap", "--yes", "--"+noLeaseFlag); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.errOut.String(), "WITHOUT the teardown lease") {
		t.Errorf("no warning: %q", tr.errOut.String())
	}
	if tr.acct.HasQueue("us-east-1", calquefootprint.PoolQueueName("m")) || tr.acct.HasRole(calquefootprint.RealRunRolePrefix+"0123456789abcdef") || tr.acct.HasObject("b", "runs/r1/manifest.json") {
		t.Errorf("litter survived: %v", tr.acct.Ops())
	}
}

func TestTeardownReapWithLease(t *testing.T) {
	tr := newTeardownRun()
	// calque has been idle in the account for 25 h.
	lc, err := lifecycle.New(lifecycle.Options{
		DynamoDB: func(string) (lifecycle.API, error) { return tr.dynamo, nil }, ControlRegion: "us-east-1",
		Now: func() time.Time { return fpNow.Add(-25 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lc.Observe(context.Background(), tags.ToolCalque, lifecycle.ScopeGlobal, false); err != nil {
		t.Fatal(err)
	}
	if err := tr.run(t, footprintJSON(t, tr.acct), "--mode", "reap", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(tr.acct.Ops()) == 0 || strings.Contains(tr.errOut.String(), "WITHOUT") {
		t.Errorf("ops %v, stderr %q", tr.acct.Ops(), tr.errOut.String())
	}
	if _, exists, _ := lc.Get(context.Background(), tags.ToolCalque, lifecycle.ScopeGlobal); exists {
		t.Error("the lease item is still there: a complete teardown finishes it")
	}
}

func TestTeardownRefusesOtherAccountsPlan(t *testing.T) {
	tr := newTeardownRun()
	plan := bytes.Replace(footprintJSON(t, tr.acct), []byte(`"account": "`+fpAcct+`"`), []byte(`"account": "999988887777"`), 1)
	err := tr.run(t, plan)
	if err == nil || !strings.Contains(err.Error(), "999988887777") {
		t.Errorf("another account's plan: %v", err)
	}
}
