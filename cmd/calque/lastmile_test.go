package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lastmile"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	"github.com/spore-host/libs/tags"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/calque/pkg/calquefootprint"
)

// lmWorld is one account, its lifecycle tables, and what the last mile logged.
type lmWorld struct {
	acct   *awstest.Account
	dynamo *lifecycletest.Fake
	dir    string

	mu     sync.Mutex
	events []string
	log    []string
}

func newLMWorld(t *testing.T) *lmWorld {
	t.Helper()
	return &lmWorld{acct: fpAccount(), dynamo: lifecycletest.New(), dir: t.TempDir()}
}

func (w *lmWorld) event(e string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, e)
}

func (w *lmWorld) debugf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, fmt.Sprintf(format, args...))
}

func (w *lmWorld) logText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.log, "\n")
}

func (w *lmWorld) eventList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

// env is the production wiring (lastmile.Run included) over the fakes.
func (w *lmWorld) env() lastMileEnv {
	return lastMileEnv{
		mode: lastmile.ModeFromEnv,
		loadAWS: func(context.Context) (aws.Config, error) {
			w.event("lookup")
			return w.acct.Config(), nil
		},
		dynamo: func(aws.Config, string) (lifecycle.API, error) { return w.dynamo, nil },
		run: func(ctx context.Context, o lastmile.Options) lastmile.Result {
			w.event("lastmile")
			return lastmile.Run(ctx, o)
		},
		now:       func() time.Time { return fpNow },
		statePath: filepath.Join(w.dir, "lifecycle.json"),
		wait:      lastMileWait,
		debugf:    w.debugf,
	}
}

// command is a stand-in calque command that exits with code.
func (w *lmWorld) command(code int) func([]string) int {
	return func([]string) int {
		w.event("command")
		return code
	}
}

// captureStdio runs fn with os.Stdout and os.Stderr redirected, and returns
// what was written to each.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	outC, errC := make(chan string, 1), make(chan string, 1)
	go func() { b, _ := io.ReadAll(or); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(er); errC <- string(b) }()
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()
	fn()
	os.Stdout, os.Stderr = origOut, origErr
	_ = ow.Close()
	_ = ew.Close()
	return <-outC, <-errC
}

var dynamoWrites = []string{"PutItem", "UpdateItem", "DeleteItem", "TransactWriteItems", "CreateTable", "DeleteTable"}

func (w *lmWorld) writes() int {
	n := 0
	for _, op := range dynamoWrites {
		n += w.dynamo.Calls(op)
	}
	return n
}

// seedStaleTeardown leaves calque's account-wide teardown lease held by a
// teardown whose last heartbeat was an hour before fpNow: stale.
func (w *lmWorld) seedStaleTeardown(t *testing.T) {
	t.Helper()
	lc, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(string) (lifecycle.API, error) { return w.dynamo, nil },
		ControlRegion: defaultLeaseRegion, Owner: "calque-teardown",
		Now: func() time.Time { return fpNow.Add(-time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := lc.Observe(ctx, tags.ToolCalque, lifecycle.ScopeGlobal, false); err != nil {
		t.Fatal(err)
	}
	if _, err := lc.Claim(ctx, tags.ToolCalque, lifecycle.ScopeGlobal, 0); err != nil {
		t.Fatal(err)
	}
}

func TestLastMileRunsAfterTheCommand(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	w := newLMWorld(t)
	if code := realMain([]string{"ami", "list"}, w.env(), w.command(0)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	ev := w.eventList()
	// The lookup may run before or alongside the command; lastmile.Run only
	// after it.
	if len(ev) != 3 || ev[len(ev)-1] != "lastmile" || !strings.Contains(strings.Join(ev, " "), "command") {
		t.Errorf("events %v, want lookup and command, then lastmile", ev)
	}
	if !strings.Contains(w.logText(), "lastmile REPORT tool=calque account="+fpAcct) {
		t.Errorf("no summary in the debug log:\n%s", w.logText())
	}
}

func TestLastMileSkippedForExcludedCommands(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	for _, args := range [][]string{
		nil, {"completion"}, {"version"}, {"help"}, {"-h"}, {"--help"}, {"footprint", "--all-regions"},
		{"teardown", "--plan", "-"}, {"session", "list"}, {"run", "x.py"}, {"no-such-command"},
	} {
		w := newLMWorld(t)
		if code := realMain(args, w.env(), w.command(3)); code != 3 {
			t.Errorf("%v: exit %d, want 3", args, code)
		}
		if ev := w.eventList(); len(ev) != 1 || ev[0] != "command" {
			t.Errorf("%v: events %v, want only the command (no lookup, no lastmile)", args, ev)
		}
	}
}

func TestLastMileRunsForEveryOtherCommand(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	for _, cmd := range []string{"analyze", "smoke", "real", "ramp", "pool", "ami", "spawn-run"} {
		w := newLMWorld(t)
		realMain([]string{cmd}, w.env(), w.command(0))
		if ev := w.eventList(); len(ev) == 0 || ev[len(ev)-1] != "lastmile" {
			t.Errorf("%s: events %v, want lastmile last", cmd, ev)
		}
	}
}

// Every command dispatch knows is in lastMileCommands, so a new command is a
// decision, not an accident.
func TestLastMileCommandsCoverDispatch(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func dispatch(")
	if start < 0 {
		t.Fatal("no dispatch in main.go")
	}
	body = body[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	cases := regexp.MustCompile(`case "([a-z-]+)":`).FindAllStringSubmatch(body, -1)
	if len(cases) < 10 {
		t.Fatalf("found only %d commands in dispatch", len(cases))
	}
	for _, c := range cases {
		if _, ok := lastMileCommands[c[1]]; !ok {
			t.Errorf("command %q is not in lastMileCommands: decide whether the last mile runs after it", c[1])
		}
	}
}

func TestLastMileOff(t *testing.T) {
	for _, v := range []string{"off", "OFF", " off "} {
		t.Setenv(lastmile.EnvVar, v)
		w := newLMWorld(t)
		realMain([]string{"ami", "list"}, w.env(), w.command(0))
		if ev := w.eventList(); len(ev) != 1 || ev[0] != "command" {
			t.Errorf("%s=%q: events %v, want only the command", lastmile.EnvVar, v, ev)
		}
		if w.dynamo.Calls("GetItem")+w.dynamo.Calls("Scan") > 0 {
			t.Errorf("%s=%q: read the lifecycle table", lastmile.EnvVar, v)
		}
	}
}

// The real lastmile.Run over a stale calque teardown: it reports, in the
// debug log only, and changes nothing.
func TestLastMileReportsStaleTeardownOnlyInTheDebugLog(t *testing.T) {
	for _, mode := range []string{"", "report", "reap"} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv(lastmile.EnvVar, mode)
			w := newLMWorld(t)
			w.seedStaleTeardown(t)
			before := w.writes()
			var code int
			stdout, stderr := captureStdio(t, func() {
				code = realMain([]string{"pool", "list"}, w.env(), w.command(0))
			})
			if code != 0 || stdout != "" || stderr != "" {
				t.Errorf("exit %d, stdout %q, stderr %q: want 0 and nothing", code, stdout, stderr)
			}
			log := w.logText()
			for _, want := range []string{
				"would resume teardown of tool#calque#global",
				// lastmile plans a scope's resources only, and global's are
				// IAM: the pool queue in us-east-1 is not listed, although
				// calque's account-wide teardown would delete it.
				"would delete " + calquefootprint.TypeRole,
				"lastmile REPORT tool=calque",
			} {
				if !strings.Contains(log, want) {
					t.Errorf("debug log lacks %q:\n%s", want, log)
				}
			}
			if n := w.writes() - before; n != 0 {
				t.Errorf("%d lifecycle write(s) in report mode", n)
			}
			if ops := w.acct.Ops(); len(ops) > 0 {
				t.Errorf("report mode changed the account: %v", ops)
			}
		})
	}
}

// Within 6 h, a second command reads only the state file.
func TestLastMileRateLimited(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	w := newLMWorld(t)
	env := w.env()
	realMain([]string{"ami", "list"}, env, w.command(0))
	reads := w.dynamo.Calls("GetItem") + w.dynamo.Calls("Scan")
	if reads == 0 {
		t.Fatal("the first run read nothing")
	}
	realMain([]string{"ami", "list"}, env, w.command(0))
	if again := w.dynamo.Calls("GetItem") + w.dynamo.Calls("Scan"); again != reads {
		t.Errorf("the second run inside 6 h read the table again (%d -> %d)", reads, again)
	}
	if !strings.Contains(w.logText(), "rate-limited") {
		t.Errorf("no rate-limit note:\n%s", w.logText())
	}
}

// deniedDynamo refuses every read, as DynamoDB does for credentials without
// dynamodb:GetItem and dynamodb:Scan on spore-host-lifecycle.
type deniedDynamo struct{ lifecycle.API }

func denied(op string) error {
	return &awstest.APIError{Code: "AccessDeniedException", Message: "not authorized to perform: dynamodb:" + op}
}

func (deniedDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return nil, denied("GetItem")
}

func (deniedDynamo) Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return nil, denied("Scan")
}

func (deniedDynamo) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return nil, denied("DescribeTable")
}

// Without the lifecycle reads, the last mile degrades to a debug line: with
// CALQUE_DEBUG unset, the user sees nothing at all.
func TestLastMileWithoutLifecyclePermissionsIsSilent(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	w := newLMWorld(t)
	env := w.env()
	env.dynamo = func(aws.Config, string) (lifecycle.API, error) { return deniedDynamo{w.dynamo}, nil }
	var code int
	stdout, stderr := captureStdio(t, func() { code = realMain([]string{"ami", "list"}, env, w.command(1)) })
	if code != 1 || stdout != "" || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q: want 1 and nothing", code, stdout, stderr)
	}
	if log := w.logText(); !strings.Contains(log, "AccessDenied") || !strings.Contains(log, "warnings=") {
		t.Errorf("the denial is not in the debug log:\n%s", log)
	}

	// With the production debug log off, nothing reaches stderr either.
	env.debugf = discardf
	stdout, stderr = captureStdio(t, func() { code = realMain([]string{"ami", "list"}, env, w.command(0)) })
	if code != 0 || stdout != "" || stderr != "" {
		t.Errorf("debug off: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestDebugEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "off": false, "1": true, "true": true, "yes": true} {
		t.Setenv("CALQUE_DEBUG", v)
		if got := debugEnabled(); got != want {
			t.Errorf("CALQUE_DEBUG=%q: %v, want %v", v, got, want)
		}
	}
}

// stdoutPrinter is a debug log that (wrongly) writes to stdout: the revert
// check that the stdout assertions below can fail at all.
func stdoutPrinter(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func TestCaptureStdioSeesAStdoutLeak(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	w := newLMWorld(t)
	env := w.env()
	env.debugf = stdoutPrinter
	stdout, _ := captureStdio(t, func() { realMain([]string{"ami", "list"}, env, w.command(0)) })
	if !strings.Contains(stdout, "lastmile") {
		t.Fatalf("a debug log on stdout went unseen (stdout %q): the no-stdout tests prove nothing", stdout)
	}
}

// Whatever goes wrong in the last mile, the exit status is the command's and
// stdout is untouched.
func TestLastMileNeverChangesExitCodeOrStdout(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "")
	release := make(chan struct{})
	defer close(release)
	faults := map[string]func(*lastMileEnv){
		"Run panics": func(e *lastMileEnv) {
			e.run = func(context.Context, lastmile.Options) lastmile.Result { panic("boom") }
		},
		"Run never returns": func(e *lastMileEnv) {
			e.run = func(context.Context, lastmile.Options) lastmile.Result { <-release; return lastmile.Result{} }
		},
		"config fails": func(e *lastMileEnv) {
			e.loadAWS = func(context.Context) (aws.Config, error) { return aws.Config{}, errors.New("no credentials") }
		},
		"config panics": func(e *lastMileEnv) {
			e.loadAWS = func(context.Context) (aws.Config, error) { panic("boom") }
		},
		"sts fails": func(e *lastMileEnv) {
			e.loadAWS = func(context.Context) (aws.Config, error) {
				return awstest.Config(awstest.Services{"STS": func(string, any) (any, error) { return nil, denied("sts") }}), nil
			}
		},
		"dynamo fails": func(e *lastMileEnv) {
			e.dynamo = func(aws.Config, string) (lifecycle.API, error) { return nil, errors.New("no client") }
		},
		"debug log panics": func(e *lastMileEnv) {
			e.debugf = func(string, ...any) { panic("log boom") }
		},
		"mode panics": func(e *lastMileEnv) {
			e.mode = func() lastmile.Mode { panic("boom") }
		},
	}
	for name, fault := range faults {
		for _, want := range []int{0, 1, 2} {
			w := newLMWorld(t)
			w.seedStaleTeardown(t)
			env := w.env()
			env.wait = time.Second
			fault(&env)
			var code int
			start := time.Now()
			stdout, _ := captureStdio(t, func() { code = realMain([]string{"real"}, env, w.command(want)) })
			if code != want || stdout != "" {
				t.Errorf("%s: exit %d (want %d), stdout %q", name, code, want, stdout)
			}
			if d := time.Since(start); d > env.wait+time.Second {
				t.Errorf("%s: took %s", name, d)
			}
		}
	}
}

func TestLastMileOptionsWiring(t *testing.T) {
	t.Setenv(lastmile.EnvVar, "reap")
	w := newLMWorld(t)
	env := w.env()
	var got lastmile.Options
	env.run = func(_ context.Context, o lastmile.Options) lastmile.Result {
		got = o
		return lastmile.Result{}
	}
	realMain([]string{"pool", "list"}, env, w.command(0))

	if got.Tool != tags.ToolCalque || got.Account != fpAcct {
		t.Errorf("tool %q account %q", got.Tool, got.Account)
	}
	if strings.Join(got.Regions, ",") != defaultLeaseRegion {
		t.Errorf("regions %v, want [%s]: calque keeps one item, scope global, there", got.Regions, defaultLeaseRegion)
	}
	if got.Lifecycle == nil {
		t.Fatal("no lifecycle client")
	}
	if r, err := got.Lifecycle.RegionFor(lifecycle.ScopeGlobal); err != nil || r != defaultLeaseRegion {
		t.Errorf("control region %q (%v), want %s", r, err, defaultLeaseRegion)
	}
	if got.Footprint == nil || got.Deleter == nil {
		t.Error("no Footprint or Deleter")
	}
	if len(got.Patterns) != len(calquefootprint.Patterns()) {
		t.Errorf("%d patterns, want calquefootprint's %d", len(got.Patterns), len(calquefootprint.Patterns()))
	}
	if len(got.Residue) != 0 {
		t.Errorf("residue %v: calque has none", got.Residue)
	}
	if got.FullVisibility || got.Activity != nil {
		t.Error("FullVisibility set: calque's backstop owns its idleness")
	}
	if got.Mode != lastmile.ModeReap {
		t.Errorf("mode %q, want SPORE_LASTMILE's (lastmile runs reap as report until libs#79)", got.Mode)
	}
	if got.Budget < lastMileMinBudget || got.Budget > lastMileWait {
		t.Errorf("budget %s, want within [%s, %s]", got.Budget, lastMileMinBudget, lastMileWait)
	}
	if got.StatePath != env.statePath || got.Logf == nil || got.Now == nil {
		t.Errorf("state %q, logf set %v, now set %v", got.StatePath, got.Logf != nil, got.Now != nil)
	}
	if err := got.Deleter.Delete(context.Background(), footprint.Resource{}); !errors.Is(err, errLastMileReportOnly) {
		t.Errorf("Deleter.Delete: %v, want a refusal", err)
	}
}

// The default env stays in budget too: production constants are bounded.
func TestLastMileBudgetIsBounded(t *testing.T) {
	if lastMileWait > 5*time.Second || lastMileMinBudget >= lastMileWait {
		t.Errorf("wait %s, min budget %s", lastMileWait, lastMileMinBudget)
	}
	if d := defaultLastMileEnv(); d.wait != lastMileWait || d.statePath != "" {
		t.Errorf("default env: wait %s, state %q", d.wait, d.statePath)
	}

	t.Setenv(lastmile.EnvVar, "")
	release := make(chan struct{})
	defer close(release)

	// A lookup that never finishes costs the command env.wait, no more.
	w := newLMWorld(t)
	env := w.env()
	env.wait = time.Second
	env.loadAWS = func(context.Context) (aws.Config, error) { <-release; return aws.Config{}, nil }
	start := time.Now()
	realMain([]string{"ami"}, env, w.command(0))
	if d := time.Since(start); d > env.wait+300*time.Millisecond {
		t.Errorf("hung lookup: the command took %s", d)
	}

	// A DynamoDB that never answers: the real lastmile.Run stops at its budget.
	w = newLMWorld(t)
	env = w.env()
	env.wait = time.Second
	env.dynamo = func(aws.Config, string) (lifecycle.API, error) { return hangingDynamo{w.dynamo, release}, nil }
	start = time.Now()
	realMain([]string{"ami"}, env, w.command(0))
	if d := time.Since(start); d > env.wait+500*time.Millisecond {
		t.Errorf("hung DynamoDB: the command took %s", d)
	}
	if !strings.Contains(w.logText(), "budget") {
		t.Errorf("no budget warning:\n%s", w.logText())
	}
}

type hangingDynamo struct {
	lifecycle.API
	release chan struct{}
}

func (h hangingDynamo) GetItem(ctx context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	<-h.release // ignores ctx, like a stuck connection
	return nil, ctx.Err()
}

// Past planBy the footprint and the re-check give up at once, so a stale
// teardown in a big account still finishes its scope inside the budget.
func TestLastMilePlannerStopsAtItsShare(t *testing.T) {
	p := &lastMilePlanner{cfg: fpAccount().Config(), account: fpAcct, now: func() time.Time { return fpNow }, planBy: time.Now().Add(-time.Second)}
	if _, err := p.footprint(context.Background()); !errors.Is(err, errLastMilePlanTime) {
		t.Errorf("footprint past planBy: %v", err)
	}
	if _, err := (lastMileDeleter{p}).Describe(context.Background(), footprint.Resource{}); !errors.Is(err, errLastMilePlanTime) {
		t.Errorf("Describe past planBy: %v", err)
	}

	p = &lastMilePlanner{cfg: fpAccount().Config(), account: fpAcct, now: func() time.Time { return fpNow }, planBy: time.Now().Add(time.Minute)}
	rep, err := p.footprint(context.Background())
	if err != nil || rep.Tool != tags.ToolCalque || rep.Account != fpAcct || len(rep.Resources) != 3 {
		t.Fatalf("footprint: %v, %+v", err, rep)
	}
}
