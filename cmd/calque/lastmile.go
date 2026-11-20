// The lazy CLI last mile (spore-host/libs#78, part of leave-no-trace
// spore-host/spore-host#752): after a calque command, libs/lastmile checks
// whether a calque teardown in this account was left half-done, and says what
// it would do about it. Report mode only: it deletes nothing and writes
// nothing to the lifecycle table, and calque's Deleter here refuses Delete
// outright.
//
// It is background hygiene, never part of the command:
//
//   - It never changes calque's exit status or writes to stdout. What it finds
//     goes to the debug log (CALQUE_DEBUG=1, on stderr) and nowhere else.
//   - It adds at most lastMileWait to a command. The account lookup it needs
//     runs alongside the command; the lifecycle reads run after it, and
//     libs/lastmile rate-limits them to once per 6 h per scope, so most
//     invocations only read ~/.spore-host/lifecycle.json.
//   - SPORE_LASTMILE=off turns it off.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go/logging"

	"github.com/spore-host/calque/pkg/calquefootprint"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lastmile"
	"github.com/spore-host/libs/lifecycle"
)

const (
	// lastMileWait is the most the last mile adds to a command, counted from
	// the moment the command returns: waiting for the account lookup if it
	// is still going, then lastmile.Run's whole budget.
	lastMileWait = 3 * time.Second
	// lastMileMinBudget: with less than this left, the run is skipped rather
	// than started and cut off.
	lastMileMinBudget = 500 * time.Millisecond
	// lastMileLookup caps the AWS config load and sts:GetCallerIdentity that
	// start with the command.
	lastMileLookup = 10 * time.Second
	// lastMileOwner names the last mile in the lifecycle client. Report mode
	// writes nothing, so it appears in no lease.
	lastMileOwner = "calque-lastmile"
)

// lastMileCommands says, for each calque command, whether the last mile runs
// after it, and why not. An unknown command (a usage error) never runs it.
var lastMileCommands = map[string]struct {
	run bool
	why string
}{
	"analyze":   {run: true},
	"smoke":     {run: true},
	"real":      {run: true},
	"ramp":      {run: true},
	"pool":      {run: true},
	"ami":       {run: true},
	"spawn-run": {run: true},

	"completion": {why: "shell completion must stay silent and instant"},
	"help":       {why: "help never touches AWS"},
	"version":    {why: "version never touches AWS, and package managers run it as an install test"},
	"footprint":  {why: "footprint is calque's own inventory"},
	"teardown":   {why: "teardown is calque's own cleanup and holds the lease itself"},
	"run":        {why: "run is local (it drives warmd on a synthetic sample) and never touches AWS"},
	"session":    {why: "session runs on an already-acquired GPU host, only reads and writes local state, and never touches AWS"},
}

// lastMileApplies reports whether the last mile runs after args, and why not.
func lastMileApplies(args []string) (bool, string) {
	if len(args) == 0 {
		return false, "no command"
	}
	c, ok := lastMileCommands[args[0]]
	if !ok {
		return false, fmt.Sprintf("%q is not a calque command", args[0])
	}
	return c.run, c.why
}

// lastMileEnv is what the last mile needs from outside, so tests can run it
// without AWS, a home directory or a real clock.
type lastMileEnv struct {
	// mode is lastmile.ModeFromEnv.
	mode func() lastmile.Mode
	// loadAWS loads the AWS config the last mile uses. Its region is
	// defaultLeaseRegion.
	loadAWS func(ctx context.Context) (aws.Config, error)
	// dynamo returns the DynamoDB client for a region.
	dynamo func(cfg aws.Config, region string) (lifecycle.API, error)
	// run is lastmile.Run.
	run func(ctx context.Context, o lastmile.Options) lastmile.Result
	// now is the clock passed to lastmile and lifecycle; nil is time.Now.
	now func() time.Time
	// statePath is lastmile's rate-limit file; "" is ~/.spore-host/lifecycle.json.
	statePath string
	// wait is lastMileWait.
	wait time.Duration
	// debugf is calque's debug log. It must never write to stdout.
	debugf func(format string, args ...any)
}

// debugEnabled reports whether CALQUE_DEBUG asks for the debug log.
func debugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CALQUE_DEBUG"))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// stderrDebugf is the debug log when CALQUE_DEBUG is set: one line on stderr.
func stderrDebugf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	_, _ = fmt.Fprintln(os.Stderr, "debug: "+msg)
}

func discardf(string, ...any) {}

func defaultLastMileEnv() lastMileEnv {
	debugf := discardf
	if debugEnabled() {
		debugf = stderrDebugf
	}
	return lastMileEnv{
		mode: lastmile.ModeFromEnv,
		loadAWS: func(ctx context.Context) (aws.Config, error) {
			// No SDK logger: the last mile must stay silent. No retries: a
			// call that fails skips the check (it is due again next
			// time), where the SDK's three attempts with backoff would
			// cost a fast command seconds when AWS is unreachable.
			return config.LoadDefaultConfig(ctx, config.WithRegion(defaultLeaseRegion),
				config.WithLogger(logging.Nop{}), config.WithRetryMaxAttempts(1))
		},
		dynamo: func(cfg aws.Config, region string) (lifecycle.API, error) {
			c := cfg.Copy()
			c.Region = region
			return dynamodb.NewFromConfig(c), nil
		},
		run:    lastmile.Run,
		wait:   lastMileWait,
		debugf: debugf,
	}
}

// startLastMile starts the account lookup for the last mile alongside the
// command, and returns the function that, once the command has returned,
// runs lastmile.Run and waits for it, for at most env.wait. Neither ever
// panics, prints to stdout, or fails.
func startLastMile(args []string, env lastMileEnv) (finish func()) {
	finish = func() {}
	defer func() {
		if p := recover(); p != nil {
			safeDebugf(env.debugf, "lastmile: internal error (recovered): %v", p)
			finish = func() {}
		}
	}()
	if ok, why := lastMileApplies(args); !ok {
		safeDebugf(env.debugf, "lastmile: skipped: %s", why)
		return finish
	}
	mode := env.mode()
	if mode == lastmile.ModeOff {
		safeDebugf(env.debugf, "lastmile: skipped: %s=off", lastmile.EnvVar)
		return finish
	}

	type lookup struct {
		cfg     aws.Config
		account string
		err     error
	}
	looked := make(chan lookup, 1)
	lctx, cancelLookup := context.WithTimeout(context.Background(), lastMileLookup)
	go func() {
		var l lookup
		defer func() {
			if p := recover(); p != nil {
				l = lookup{err: fmt.Errorf("account lookup: internal error (recovered): %v", p)}
			}
			looked <- l
		}()
		if l.cfg, l.err = env.loadAWS(lctx); l.err != nil {
			l.err = fmt.Errorf("load AWS config: %w", l.err)
			return
		}
		l.account, l.err = callerAccount(lctx, l.cfg)
	}()

	return func() {
		defer cancelLookup()
		defer func() {
			if p := recover(); p != nil {
				safeDebugf(env.debugf, "lastmile: internal error (recovered): %v", p)
			}
		}()
		deadline := time.Now().Add(env.wait)
		var l lookup
		select {
		case l = <-looked:
		case <-time.After(env.wait):
			safeDebugf(env.debugf, "lastmile: skipped: the account lookup took longer than %s", env.wait)
			return
		}
		if l.err != nil {
			safeDebugf(env.debugf, "lastmile: skipped: %v", l.err)
			return
		}
		budget := time.Until(deadline)
		if budget < lastMileMinBudget {
			safeDebugf(env.debugf, "lastmile: skipped: only %s of its %s left after the account lookup", budget.Round(time.Millisecond), env.wait)
			return
		}
		runLastMile(l.cfg, l.account, mode, budget, env)
	}
}

// runLastMile runs lastmile.Run with budget, and returns when it does or once
// the budget is spent, whichever comes first.
func runLastMile(cfg aws.Config, account string, mode lastmile.Mode, budget time.Duration, env lastMileEnv) {
	o, err := lastMileOptions(cfg, account, mode, budget, env)
	if err != nil {
		safeDebugf(env.debugf, "lastmile: skipped: %v", err)
		return
	}
	// lastmile.Run enforces budget itself (and records a scope it cuts off
	// as "budget exceeded"); this deadline and the select below are the
	// backstop for the case it does not return.
	hardStop := budget + 250*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), hardStop)
	defer cancel()
	done := make(chan lastmile.Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- lastmile.Result{Mode: mode, Tool: calquefootprint.Tool, Account: account,
					SkippedReason: fmt.Sprintf("internal error (recovered): %v", p)}
			}
		}()
		done <- env.run(ctx, o)
	}()
	select {
	case res := <-done:
		safeDebugf(env.debugf, "%s", res.Summary())
	case <-time.After(hardStop):
		safeDebugf(env.debugf, "lastmile: abandoned after its %s budget", budget.Round(time.Millisecond))
	}
}

// safeDebugf logs through debugf, which must not take the command down.
func safeDebugf(debugf func(string, ...any), format string, args ...any) {
	if debugf == nil {
		return
	}
	defer func() { _ = recover() }()
	debugf(format, args...)
}

// lastMileOptions is calque's lastmile.Options.
//
// Regions is defaultLeaseRegion alone, not every enabled region. calque keeps
// exactly one lifecycle item, the account-wide teardown lease (scope global,
// in --lease-region, defaultLeaseRegion by default), and writes no per-region
// item; it has no residue either. So the global scope, which ControlRegion
// gives, is the only one with anything to find, and the region scope only
// adds the empty-table check for that same table. Every enabled region would
// cost a DescribeRegions on every invocation plus a GetItem and a Scan in
// each of ~17 regions every 6 h, all to read items calque never writes; the
// regions where calque resources were found would need the full inventory
// first. A teardown run with a non-default --lease-region is not seen.
//
// FullVisibility is false. calque's idleness belongs to its backstop, the
// spawn reaper's calque module (spawn#1654), and calque's footprint does not
// see every calque activity signal: an acquisition still waiting for
// capacity has no instance yet, a run's S3 objects are visible only in the
// buckets named with --bucket, and a session's slice holds are local state on
// the GPU host.
func lastMileOptions(cfg aws.Config, account string, mode lastmile.Mode, budget time.Duration, env lastMileEnv) (lastmile.Options, error) {
	lc, err := lifecycle.New(lifecycle.Options{
		DynamoDB:      func(region string) (lifecycle.API, error) { return env.dynamo(cfg, region) },
		ControlRegion: defaultLeaseRegion,
		Owner:         lastMileOwner,
		Now:           env.now,
	})
	if err != nil {
		return lastmile.Options{}, err
	}
	p := &lastMilePlanner{cfg: cfg, account: account, now: env.now, planBy: time.Now().Add(budget / 2)}
	return lastmile.Options{
		Tool:           calquefootprint.Tool,
		Account:        account,
		Regions:        []string{defaultLeaseRegion},
		Lifecycle:      lc,
		Footprint:      p.footprint,
		Deleter:        lastMileDeleter{p},
		Patterns:       calquefootprint.Patterns(),
		Residue:        nil,
		FullVisibility: false,
		Mode:           mode,
		Now:            env.now,
		StatePath:      env.statePath,
		Budget:         budget,
		Logf:           env.debugf,
	}, nil
}

// errLastMilePlanTime is what the footprint and the re-check return once the
// part of the budget set aside for them is spent.
var errLastMilePlanTime = errors.New("past the last mile's time limit; `calque footprint --all-regions` lists everything")

// lastMilePlanner gives lastmile calque's footprint and Deleter, needed only
// to list what a stale teardown would still delete. Both list every enabled
// region, which does not fit the last mile's few seconds in most accounts,
// so both stop at planBy (half the budget) and fail fast after it. lastmile
// then reports the stale teardown with a warning instead of a list, and the
// scope still finishes inside the budget: a cut-off scope is retried on every
// invocation, so a slow inventory would otherwise delay every command until
// the teardown was resumed.
type lastMilePlanner struct {
	cfg     aws.Config
	account string
	now     func() time.Time
	planBy  time.Time

	once       sync.Once
	clients    calquefootprint.Clients
	regions    []string
	regionsErr error
	deleter    footprint.Deleter
}

func (p *lastMilePlanner) setup(ctx context.Context) error {
	p.once.Do(func() {
		p.clients = calquefootprint.ClientsFromConfig(p.cfg)
		p.regions, p.regionsErr = calquefootprint.EnabledRegions(ctx, ec2.NewFromConfig(p.cfg))
		p.deleter = calquefootprint.NewDeleter(p.clients, calquefootprint.DeleterOptions{Regions: p.regions, Now: p.now})
	})
	return p.regionsErr
}

func (p *lastMilePlanner) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *lastMilePlanner) footprint(ctx context.Context) (*footprint.Report, error) {
	ctx, cancel := context.WithDeadline(ctx, p.planBy)
	defer cancel()
	if err := p.setup(ctx); err != nil {
		return nil, timeLimited(ctx, err)
	}
	fp, err := calquefootprint.Inventory(ctx, p.clients, calquefootprint.Options{
		Account: p.account, Regions: p.regions, AllRegions: true,
	}, p.clock())
	if err != nil {
		return nil, timeLimited(ctx, err)
	}
	if ctx.Err() != nil {
		return nil, errLastMilePlanTime
	}
	return fp.Report, nil
}

// timeLimited is err, or errLastMilePlanTime if ctx's deadline caused it.
func timeLimited(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", errLastMilePlanTime, err)
	}
	return err
}

// lastMileDeleter re-checks resources with calque's Deleter, until planBy.
// It never deletes: the last mile is report mode only.
type lastMileDeleter struct{ p *lastMilePlanner }

func (d lastMileDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	ctx, cancel := context.WithDeadline(ctx, d.p.planBy)
	defer cancel()
	if ctx.Err() != nil {
		return r, errLastMilePlanTime
	}
	if err := d.p.setup(ctx); err != nil {
		return r, timeLimited(ctx, err)
	}
	out, err := d.p.deleter.Describe(ctx, r)
	if err != nil {
		return out, timeLimited(ctx, err)
	}
	return out, nil
}

// errLastMileReportOnly is Delete's answer.
var errLastMileReportOnly = errors.New("the last mile is report mode only: it deletes nothing")

func (lastMileDeleter) Delete(context.Context, footprint.Resource) error {
	return errLastMileReportOnly
}
