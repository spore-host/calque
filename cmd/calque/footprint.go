// calque footprint / calque teardown: list what calque created in an account,
// and remove what is safe to remove (calque#228, part of leave-no-trace,
// spore-host/spore-host#752).
//
// footprint is read-only. teardown takes a footprint as its plan and runs it
// through the shared executor (libs/footprint.Execute) in report mode (the
// default: say what would be deleted) or reap mode (delete it), with calque's
// Deleter re-checking every resource before it is touched.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/spore-host/calque/pkg/calquefootprint"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
)

// listFlag is a repeatable flag that also splits on commas.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*l = append(*l, v)
		}
	}
	return nil
}

type footprintOpts struct {
	region     string
	allRegions bool
	buckets    []string
	output     string
	maxObjects int
}

func parseFootprintArgs(args []string) (footprintOpts, error) {
	fs := flag.NewFlagSet("footprint", flag.ContinueOnError)
	region := fs.String("region", "us-east-1", "AWS region to list (IAM is global and always listed)")
	all := fs.Bool("all-regions", false, "list every region enabled in the account (needed before IAM roles can be judged unused)")
	var buckets listFlag
	fs.Var(&buckets, "bucket", "S3 bucket calque wrote into (repeatable). calque records no buckets, so it only looks in the ones named here, under runs/, fleet/ and ami-bake/")
	out := fs.String("o", "table", "output: table or json")
	maxObjects := fs.Int("max-objects", 10000, "most S3 objects to examine per bucket (one GetObjectTagging each)")
	if err := fs.Parse(args); err != nil {
		return footprintOpts{}, err
	}
	explicitRegion := false
	fs.Visit(func(f *flag.Flag) { explicitRegion = explicitRegion || f.Name == "region" })
	if *all && explicitRegion {
		return footprintOpts{}, errors.New("--region and --all-regions are exclusive")
	}
	if *out != "table" && *out != "json" {
		return footprintOpts{}, fmt.Errorf("-o %q: want table or json", *out)
	}
	if fs.NArg() > 0 {
		return footprintOpts{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return footprintOpts{region: *region, allRegions: *all, buckets: buckets, output: *out, maxObjects: *maxObjects}, nil
}

func footprintCmd(args []string) error {
	o, err := parseFootprintArgs(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	return runFootprint(ctx, cfg, o, os.Stdout, time.Now())
}

// callerAccount is the account the credentials in cfg belong to.
func callerAccount(ctx context.Context, cfg aws.Config) (string, error) {
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("resolve account id: %w", err)
	}
	return aws.ToString(id.Account), nil
}

func runFootprint(ctx context.Context, cfg aws.Config, o footprintOpts, w io.Writer, now time.Time) error {
	account, err := callerAccount(ctx, cfg)
	if err != nil {
		return err
	}
	regions := []string{o.region}
	if o.allRegions {
		if regions, err = calquefootprint.EnabledRegions(ctx, ec2.NewFromConfig(cfg)); err != nil {
			return err
		}
	}
	fp, err := calquefootprint.Inventory(ctx, calquefootprint.ClientsFromConfig(cfg), calquefootprint.Options{
		Account: account, Regions: regions, AllRegions: o.allRegions, Buckets: o.buckets, MaxObjects: o.maxObjects,
	}, now)
	if err != nil {
		return err
	}
	if o.output == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(fp)
	}
	return printFootprint(w, fp)
}

func printFootprint(w io.Writer, fp *calquefootprint.Footprint) error {
	rep := fp.Report
	_, _ = fmt.Fprintf(w, "calque footprint in %s (%s)\n\n", rep.Account, rep.GeneratedAt.Format(time.RFC3339))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TYPE\tREGION\tNAME\tOWNERSHIP\tCLASS\tDELETABLE\tDETAIL")
	for _, r := range rep.Resources {
		d := fp.Details[r.ARN]
		detail := d.Why
		if q := d.Queue; q != nil {
			detail = fmt.Sprintf("%s (messages %d visible, %d in flight, %d delayed; workers %d)", d.Why, q.Visible, q.InFlight, q.Delayed, len(q.Workers))
		}
		if !r.Deletable && r.Reason != "" {
			detail += "; kept: " + r.Reason
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\t%s\n", strings.TrimPrefix(r.Type, "AWS::"), r.Region, r.Name, r.Ownership, r.Class, r.Deletable, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(rep.Resources) == 0 {
		_, _ = fmt.Fprintln(w, "(nothing found)")
	}
	for _, warn := range rep.Warnings {
		_, _ = fmt.Fprintln(w, "warning:", warn)
	}
	return nil
}

type teardownOpts struct {
	plan        string
	mode        footprint.Mode
	yes         bool
	noLease     bool
	leaseRegion string
	output      string
}

// defaultLeaseRegion is where calque keeps its one lifecycle item (scope
// global): the --lease-region default, and where the last mile looks.
const defaultLeaseRegion = "us-east-1"

// noLeaseFlag is hidden from --help: it is a last resort, not an option.
const noLeaseFlag = "i-understand-no-lease"

func parseTeardownArgs(args []string) (teardownOpts, error) {
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	plan := fs.String("plan", "", "footprint JSON to act on (`calque footprint -o json`), a file or - for stdin (required)")
	mode := fs.String("mode", string(footprint.ModeReport), "report (say what would be deleted) or reap (delete it)")
	yes := fs.Bool("yes", false, "required with --mode reap")
	noLease := fs.Bool(noLeaseFlag, false, "reap without the shared-account teardown lease")
	leaseRegion := fs.String("lease-region", defaultLeaseRegion, "region of the spore-host-lifecycle table holding calque's account-wide teardown lease")
	out := fs.String("o", "table", "output: table or json")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: calque teardown --plan <file|-> [--mode report|reap] [--yes] [--lease-region R] [-o table|json]")
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name == noLeaseFlag {
				return
			}
			_, _ = fmt.Fprintf(fs.Output(), "  -%s\n    \t%s (default %q)\n", f.Name, f.Usage, f.DefValue)
		})
	}
	if err := fs.Parse(args); err != nil {
		return teardownOpts{}, err
	}
	o := teardownOpts{plan: *plan, mode: footprint.Mode(*mode), yes: *yes, noLease: *noLease, leaseRegion: *leaseRegion, output: *out}
	switch {
	case o.plan == "":
		return o, errors.New("usage: calque teardown --plan <file|-> [--mode report|reap] [--yes]")
	case o.mode != footprint.ModeReport && o.mode != footprint.ModeReap:
		return o, fmt.Errorf("--mode %q: want report or reap", *mode)
	case o.output != "table" && o.output != "json":
		return o, fmt.Errorf("-o %q: want table or json", o.output)
	case fs.NArg() > 0:
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

// teardownEnv is what runTeardown needs from outside: AWS, the lease table's
// DynamoDB, the clock, stdin and stdout.
type teardownEnv struct {
	cfg    aws.Config
	dynamo func(region string) (lifecycle.API, error)
	now    func() time.Time
	stdin  io.Reader
	out    io.Writer
	errOut io.Writer
}

func teardownCmd(args []string) error {
	o, err := parseTeardownArgs(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.leaseRegion))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	return runTeardown(ctx, o, teardownEnv{
		cfg: cfg,
		dynamo: func(region string) (lifecycle.API, error) {
			c := cfg.Copy()
			c.Region = region
			return dynamodb.NewFromConfig(c), nil
		},
		now: time.Now, stdin: os.Stdin, out: os.Stdout, errOut: os.Stderr,
	})
}

func readPlan(path string, stdin io.Reader) (*footprint.Report, error) {
	rd := stdin
	if path != "-" {
		f, err := os.Open(path) // #nosec G304 -- the operator names the plan file
		if err != nil {
			return nil, fmt.Errorf("open plan: %w", err)
		}
		defer func() { _ = f.Close() }()
		rd = f
	}
	rep, err := footprint.Decode(rd)
	if err != nil {
		return nil, err
	}
	if rep.Tool != calquefootprint.Tool {
		return nil, fmt.Errorf("plan is %s's footprint, not calque's", rep.Tool)
	}
	return rep, nil
}

func runTeardown(ctx context.Context, o teardownOpts, env teardownEnv) error {
	rep, err := readPlan(o.plan, env.stdin)
	if err != nil {
		return err
	}
	account, err := callerAccount(ctx, env.cfg)
	if err != nil {
		return err
	}
	if rep.Account != account {
		return fmt.Errorf("plan is for account %q, but these credentials are for %s", rep.Account, account)
	}
	if o.mode == footprint.ModeReap && !o.yes {
		return errors.New("refusing to reap: pass --yes (deletes every deletable resource in the plan that is still idle)")
	}
	plan, err := footprint.NewPlan(calquefootprint.Tool, calquefootprint.Patterns(), rep.Resources)
	if err != nil {
		return err
	}
	regions, err := calquefootprint.EnabledRegions(ctx, ec2.NewFromConfig(env.cfg))
	if err != nil {
		return err
	}
	deleter := calquefootprint.NewDeleter(calquefootprint.ClientsFromConfig(env.cfg), calquefootprint.DeleterOptions{Regions: regions, Now: env.now})

	var lease *lifecycle.Lease
	shared := false
	if o.mode == footprint.ModeReap {
		lease, shared, err = teardownLease(ctx, o, env)
		if err != nil {
			return err
		}
	}
	res, execErr := footprint.Execute(ctx, plan, o.mode, deleter, lease)
	if res == nil {
		return execErr
	}
	failed := realFailures(res)
	if shared {
		// The lease is done either way: Finish when everything deletable
		// went, Release (the grace starts over) when anything was in use or
		// failed, so the next teardown re-judges.
		var lerr error
		if complete(res) {
			lerr = lease.Finish(ctx)
		} else {
			lerr = lease.Release(ctx)
		}
		if lerr != nil {
			_, _ = fmt.Fprintf(env.errOut, "warning: finishing the teardown lease: %v\n", lerr)
		}
	}
	if o.output == "json" {
		enc := json.NewEncoder(env.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else if err := printResult(env.out, res); err != nil {
		return err
	}
	if errors.Is(execErr, lifecycle.ErrLeaseLost) {
		return fmt.Errorf("teardown stopped: the lease was lost (someone started using calque in this account): %w", execErr)
	}
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed: %w", failed, execErr)
	}
	return nil
}

// complete reports whether every step ended with its resource gone or kept by
// the guard (user data, a legacy name): nothing in use, failed or cut short.
func complete(res *footprint.Result) bool {
	for _, a := range res.Actions {
		switch a.Outcome {
		case footprint.OutcomeDeleted, footprint.OutcomeGone, footprint.OutcomeKept, footprint.OutcomeWouldDelete:
		default:
			return false
		}
	}
	return true
}

// realFailures counts the failed steps that were not just a resource found
// in use (calquefootprint.ErrActive), which is a normal outcome.
func realFailures(res *footprint.Result) int {
	n := 0
	for _, a := range res.Actions {
		if (a.Outcome == footprint.OutcomeFailed || a.Outcome == footprint.OutcomeStopped) && !errors.Is(a.Err, calquefootprint.ErrActive) {
			n++
		}
	}
	return n
}

// teardownLease claims calque's account-wide teardown lease in the lifecycle
// table (libs/lifecycle, scope global, in --lease-region). shared reports
// whether it is the real, shared lease.
//
// Without it, reap refuses, unless --i-understand-no-lease: then the run gets
// a lease over a process-local, in-memory table, which satisfies the executor
// and fences nothing. Nothing then stops another user of the account from
// creating calque resources while this deletes; every resource is still
// re-checked for activity just before it is deleted.
func teardownLease(ctx context.Context, o teardownOpts, env teardownEnv) (*lifecycle.Lease, bool, error) {
	c, err := lifecycle.New(lifecycle.Options{DynamoDB: env.dynamo, ControlRegion: o.leaseRegion, Owner: "calque-teardown", Now: env.now})
	if err != nil {
		return nil, false, err
	}
	lease, err := c.Claim(ctx, calquefootprint.Tool, lifecycle.ScopeGlobal, calquefootprint.Grace)
	if err == nil {
		return lease, true, nil
	}
	if !o.noLease {
		return nil, false, fmt.Errorf("refusing to reap without calque's teardown lease (%v). "+
			"The lease is what stops this teardown racing someone else's calque run in a shared account", err)
	}
	_, _ = fmt.Fprintf(env.errOut, "WARNING: reaping WITHOUT the teardown lease (%v): nothing stops a concurrent calque run in this account; "+
		"each resource is still re-checked for activity just before it is deleted.\n", err)
	lease, err = localLease(ctx, env.now)
	return lease, false, err
}

// localLease is a lease over a fresh in-memory lifecycle table: it never
// fails a Step, because nobody else can see it.
func localLease(ctx context.Context, now func() time.Time) (*lifecycle.Lease, error) {
	f := lifecycletest.New()
	c, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(string) (lifecycle.API, error) { return f, nil },
		ControlRegion: "local", Owner: "calque-teardown/no-lease", Now: now,
	})
	if err != nil {
		return nil, err
	}
	if _, _, err := c.Observe(ctx, calquefootprint.Tool, lifecycle.ScopeGlobal, false); err != nil {
		return nil, fmt.Errorf("local lease: %w", err)
	}
	lease, err := c.Claim(ctx, calquefootprint.Tool, lifecycle.ScopeGlobal, 0)
	if err != nil {
		return nil, fmt.Errorf("local lease: %w", err)
	}
	return lease, nil
}

func printResult(w io.Writer, res *footprint.Result) error {
	_, _ = fmt.Fprintf(w, "calque teardown (%s mode)\n\n", res.Mode)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OUTCOME\tTYPE\tNAME\tREASON")
	counts := map[string]int{}
	for _, a := range res.Actions {
		outcome, reason := string(a.Outcome), a.Reason
		if a.Outcome == footprint.OutcomeFailed && errors.Is(a.Err, calquefootprint.ErrActive) {
			outcome, reason = "in-use", errText(a.Err)
		} else if a.Err != nil {
			reason += ": " + a.Err.Error()
		}
		counts[outcome]++
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", outcome, strings.TrimPrefix(a.Type, "AWS::"), a.Name, reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	var parts []string
	for k, v := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(parts)
	_, _ = fmt.Fprintf(w, "\n%s\n", strings.Join(parts, ", "))
	return nil
}

// errText is the in-use reason without the executor's and the deleter's
// prefixes.
func errText(err error) string {
	s := err.Error()
	if i := strings.Index(s, calquefootprint.ErrActive.Error()+": "); i >= 0 {
		return s[i+len(calquefootprint.ErrActive.Error())+2:]
	}
	return s
}
