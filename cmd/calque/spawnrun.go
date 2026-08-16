package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/parse"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// spawnOpts controls a spawnRun invocation: block-and-wait fan-out over every
// .spawn()-classified callable in app, one acquired instance per DISTINCT
// callable (mirroring fleetRun's one-instance-per-shard shape, but sharded by
// callable identity instead of item-index range — calque#97's original
// scope, built on #110's string-keyed collector and #111's callable
// resolver).
type spawnOpts struct {
	bucket   string
	region   string
	runID    string
	instance string // one instance type for every spawned callable (homogeneous fleet, matching fleetRun's own scope)
	ami      string
	ttl      string
	deadline time.Duration
	ratesFP  string
}

// spawnRun is the .spawn()/.get() block-and-wait fan-out driver (calque#97,
// #112): the top-level entry point sibling to fleetRun (fleetrun.go:37) —
// same acquire/parallel/re-drive/collect shape, but built around
// ir.App.FindFunction/FindClass-resolved callables (calque#111) instead of
// one callable's item range, and calexec.CollectNamedShards (calque#110)
// instead of CollectShards.
//
// This is a PARALLEL path around run.go's single-warmUnit model, not an
// extension of pickWarmUnit — per this issue's own scope, run() is built
// entirely around exactly one warmUnit and cannot express "run N different
// callables' bodies in one invocation." spawnRun never calls pickWarmUnit.
//
// sites carries every .spawn() call site's target + arguments — this is
// PARSE-LAYER data (from Modal source, not yet threaded from
// pyInvokeCall.Args through to a convenient ir.App-level API by any prior
// issue in this milestone), so the caller (cmd's spawnCmd, wiring the CLI)
// is responsible for building it from the same app it parsed.
func spawnRun(ctx context.Context, app ir.App, sites []calexec.SpawnCallSite, o spawnOpts) (err error) {
	rep := &leak.Report{}
	callables := calexec.ResolveSpawnCallables(app)
	if len(callables) == 0 {
		return fmt.Errorf("no .spawn()-classified callables found in %s — nothing to fan out", app.Script)
	}
	fmt.Printf("=== calque SPAWN fan-out (%d callable(s), region=%s instance=%s) ===\n", len(callables), o.region, o.instance)

	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}

	base := "spawn/" + o.runID
	sharedLayout := calexec.RunLayout{Bucket: o.bucket, ArtifactPfx: base + "/artifacts"}
	if err := calexec.UploadArtifacts(ctx, s3c, sharedLayout, warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[spawn] artifacts uploaded; building %d named shard(s)\n", len(callables))

	shs := calexec.BuildSpawnManifests(callables, sites, base, sharedLayout.ArtifactPfx)
	if len(shs) == 0 {
		return fmt.Errorf("no .spawn() call sites matched any classified callable — nothing to run")
	}
	byKey := make(map[string]calexec.SpawnCallable, len(callables))
	for _, c := range callables {
		byKey[c.Key] = c
	}

	// Shared EC2 client (stateless, safe for concurrent shard goroutines) — used
	// both for the AZ sweep below and each shard's spawn#497 liveness check.
	ec2c := ec2.NewFromConfig(cfg)
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2c, o.instance); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	// One acquire+run per named shard, IN PARALLEL — mirrors fleetRun's D2 exactly,
	// sharded by callable key instead of item-index range.
	var repMu sync.Mutex
	safeRep := &syncReport{rep: rep, mu: &repMu}
	shardErrs := make([]error, len(shs))
	var wg sync.WaitGroup
	for i := range shs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			shardErrs[i] = runSpawnShard(ctx, app, s3c, ec2c, spawnClient, o, shs[i], byKey[shs[i].Key], places, safeRep)
		}(i)
	}
	wg.Wait()

	// Re-drive a failed shard ONCE, mirroring fleetRun's D4.
	for i := range shs {
		if shardErrs[i] != nil {
			fmt.Fprintf(os.Stderr, "[spawn] shard %q failed (%v); re-driving once on a fresh instance\n", shs[i].Key, shardErrs[i])
			serr := runSpawnShard(ctx, app, s3c, ec2c, spawnClient, o, shs[i], byKey[shs[i].Key], places, safeRep)
			shardErrs[i] = serr
			if serr != nil {
				safeRep.Addf(leak.PrimAcquire, leak.KindSemanticGap, app.Script, 0,
					"spawn shard %q permanently failed after one re-drive (%v); its results surface in global missing[]", shs[i].Key, serr)
			}
		}
	}

	results, missing, err := calexec.CollectNamedShards(ctx, s3c, o.bucket, shs)
	if err != nil {
		return fmt.Errorf("collect named shards: %w", err)
	}
	fmt.Printf("[spawn] collected results for %d/%d callable(s) (%d missing)\n", len(results)-len(missing), len(shs), len(missing))
	for key, res := range results {
		fmt.Printf("      %s: %d result(s)\n", key, len(res))
	}
	if len(missing) > 0 {
		safeRep.Addf(leak.PrimMap, leak.KindSemanticGap, app.Script, 0,
			"%d of %d spawned callable(s) did not fully complete: %v (partial failure §10)", len(missing), len(shs), missing)
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// spawnManifestBody builds the calexec.ManifestBody runSpawnShard writes
// for callable — factored out as a pure function (no ctx/S3) so the
// calque#191 splat-routing decision is unit-testable without live AWS,
// mirroring items.go's manifestBodyForUnit.
//
// calque#191: a callable with 2+ real args (e.g. run_forecast_inference(
// job_id, model_id, config)) needs runner.py's starmap-splat path to bind
// every name, not just the first — spawnArgsPayload
// (internal/exec/spawnshard.go) already packs a multi-arg call site's
// payload into a list; without Starmap=true + MethodArgs set here, that
// list arrived at a single-param __calque_method__ and every name past
// the first stayed undefined on real hardware. len(callable.MethodArgs)
// <= 1 (the overwhelmingly common case — a plain single-arg spawned
// callable) reproduces the pre-#191 manifest byte-for-byte.
//
// calque#198: extras/extraConsts/extraImports/extraClasses (resolved by
// warmUnitForSpawnCallable + collectLocalExtras, the SAME transitive-
// closure resolution manifestBodyForUnit already uses for real/fleetrun)
// ship every sibling function/constant/import/class the callable's body
// bare-references — e.g. AI-Almanac's forecasts_app.py's
// run_season_forecast_bundle delegating to a private module-level
// _season_bundle_impl helper. Before this, SpawnCallable.MethodBody was
// shipped 100% verbatim with NO sibling resolution at all — a NameError
// on real hardware for any spawned callable using this completely
// ordinary Python pattern (thin @app.function wrapper, real logic in a
// plain helper).
func spawnManifestBody(callable calexec.SpawnCallable, extras []warm.ExtraFunc, extraConsts []warm.ExtraConst, extraImports []warm.ExtraImport, extraClasses []warm.ExtraClass) calexec.ManifestBody {
	arg := callable.MethodArg
	if arg == "" {
		arg = "item"
	}
	body := calexec.ManifestBody{
		EnterBody: callable.EnterBody, MethodBody: callable.MethodBody, MethodArg: arg,
		Extras: extras, ExtraConsts: extraConsts, ExtraImports: extraImports, ExtraClasses: extraClasses,
	}
	if len(callable.MethodArgs) > 1 {
		body.MethodArgs = callable.MethodArgs
		body.Starmap = true
	}
	return body
}

// warmUnitForSpawnCallable resolves callable's OWN ir.Function/ir.Class
// back out of app (calque#198) — SpawnCallable only carries the flattened
// fields ResolveSpawnCallables already extracted; collectLocalExtras
// needs the full warmUnit shape (LocalCalls/FreeRefs live on ir.Function/
// ir.Class, not on SpawnCallable). false if callable.Key names neither a
// Function nor a Class method in app — should not normally happen, since
// callable came from ResolveSpawnCallables(app) in the first place, but
// handled defensively rather than assumed.
func warmUnitForSpawnCallable(app ir.App, callable calexec.SpawnCallable) (warmUnit, bool) {
	if !callable.IsClass {
		if f, ok := app.FindFunction(callable.Key); ok {
			return warmUnit{method: f, plainFunction: true}, true
		}
		return warmUnit{}, false
	}
	for _, c := range app.Classes {
		for _, m := range c.Methods {
			if m.Name == callable.Key {
				return warmUnit{class: c, method: m}, true
			}
		}
	}
	return warmUnit{}, false
}

// runSpawnShard acquires ONE instance for one named shard, writes its
// manifest (using THIS callable's own EnterBody/MethodBody — the point of
// calque#111's resolver, replacing fleetRun's shared realEnterBody/
// realMethodBody constants), drives warmd HOST-MODE (no docker/GPU
// assumption — a .spawn()'d callable is, in general, a plain CPU function
// like testdata/scripts/spawn_fanout.py's worker_a/worker_b, not
// necessarily a vLLM inference call), waits for the summary, and
// terminates. Mirrors fleetRun's runShard structurally.
func runSpawnShard(ctx context.Context, app ir.App, s3c *s3.Client, ec2c *ec2.Client, spawnClient *spawnaws.Client, o spawnOpts,
	sh calexec.NamedShard, callable calexec.SpawnCallable, places []plan.Placement, rep *syncReport) error {
	shardLayout := calexec.RunLayout{
		Bucket: o.bucket, ArtifactPfx: "spawn/" + o.runID + "/artifacts",
		ManifestKey: sh.ManifestKey, ResultPrefix: sh.ResultPrefix, SummaryKey: sh.SummaryKey, LogKey: sh.LogKey,
	}
	// calque#198: resolve this callable's own sibling functions/constants/
	// imports/classes the same way manifestBodyForUnit already does for
	// real/fleetrun — MethodBody is shipped verbatim, so any bare
	// reference to a module-level helper (e.g. a thin @app.function
	// wrapper delegating to a private _impl function, AI-Almanac's exact
	// forecasts_app.py shape) must be resolved here or it NameErrors on
	// real hardware.
	var extras []warm.ExtraFunc
	var extraConsts []warm.ExtraConst
	var extraImports []warm.ExtraImport
	var extraClasses []warm.ExtraClass
	if unit, ok := warmUnitForSpawnCallable(app, callable); ok {
		extras, extraConsts, extraImports, extraClasses = collectLocalExtras(app, unit, rep.rep)
	}
	body := spawnManifestBody(callable, extras, extraConsts, extraImports, extraClasses)
	if err := calexec.WriteManifestBody(ctx, s3c, shardLayout, body, hostWorkerDir, sh.Items, nil, nil); err != nil {
		return fmt.Errorf("shard %q write manifest: %w", sh.Key, err)
	}
	boot := calexec.BootstrapConfig{
		Bucket: o.bucket, ArtifactPrefix: shardLayout.ArtifactPfx, ManifestKey: shardLayout.ManifestKey,
		WorkerDir: hostWorkerDir, Region: o.region, LogKey: shardLayout.LogKey, HostMode: true,
	}
	// calque#148: see realrun.go's identical fix — without this, the
	// spawned instance has no credentials for its own bootstrap's aws s3
	// cp/sync calls, including its own failure log.
	iamProfile, err := plan.RealRunInstanceProfile(ctx, spawnClient, o.region, o.bucket)
	if err != nil {
		return fmt.Errorf("shard %q set up IAM instance profile: %w", sh.Key, err)
	}
	launchCfg := plan.SpawnLauncher{
		RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", AMI: o.ami,
		IamInstanceProfile: iamProfile,
		RunID:              o.runID, Command: "spawn-run",
	}.Build()
	acq := &plan.Acquirer{LaunchConfig: launchCfg, Report: rep.rep, Deadline: o.deadline, Placements: places}
	// calque#134: carry THIS callable's own requested card (parsed at #111's
	// resolver into SpawnCallable.GPU) through Recommend, instead of always
	// hardcoding DefaultCard regardless of what the .spawn()'d callable asked
	// for. Most .spawn()'d callables are plain CPU functions with no gpu= at
	// all (GPU == ""), in which case Recommend's own fallback applies.
	tgt := target.StubRecommender{}.Recommend(ir.App{}, ir.Function{GPU: callable.GPU})
	tgt.Instance = o.instance
	acquired, err := acq.Acquire(ctx, &tgt, o.region)
	if err != nil {
		return fmt.Errorf("shard %q acquire: %w", sh.Key, err)
	}
	fmt.Printf("[spawn] shard %q landed %s (%s) after %s\n", sh.Key, acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))
	defer func() {
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "[spawn] WARNING: shard %q terminate failed for %s: %v (TTL reaps)\n", sh.Key, acquired.InstanceID, tErr)
		}
	}()

	// spawn#497 (spored v0.100.0+): also check spawn:last-heartbeat, so a
	// genuinely hung/gone shard fails fast instead of dead-waiting the whole
	// deadline.
	_, err = calexec.WaitForSummaryLiveness(ctx, s3c, ec2c, acquired.InstanceID, shardLayout, o.deadline, 15*time.Second, staleHeartbeatAfter, nil)
	if err != nil {
		var stale *calexec.ErrInstanceStale
		if errors.As(err, &stale) {
			return fmt.Errorf("shard %q went unresponsive mid-run (%w)", sh.Key, stale)
		}
		var bf *calexec.ErrBootstrapFailed
		if errors.As(err, &bf) {
			return fmt.Errorf("shard %q bootstrap failed", sh.Key)
		}
		return fmt.Errorf("shard %q wait summary: %w", sh.Key, err)
	}
	return nil
}

// spawnRunCmd wires the `calque spawn-run` CLI verb, gated behind
// --i-understand-this-spends-money like every other billable calque command
// (main.go's smoke/real/session/pool all follow this exact pattern).
func spawnRunCmd(args []string) error {
	fs := flag.NewFlagSet("spawn-run", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	region := fs.String("region", "us-east-1", "AWS region")
	runID := fs.String("run-id", "", "unique run id (required)")
	instance := fs.String("instance", "m7i.large", "instance type for every spawned callable (homogeneous fleet — .spawn()'d callables are typically plain CPU functions, not GPU inference, see testdata/scripts/spawn_fanout.py)")
	ami := fs.String("ami", "", "pinned AMI (required — spawn's GPU auto-select workaround doesn't apply to a CPU instance type, but auto-select for CPU types has its own known issues; pin for determinism)")
	ttl := fs.String("ttl", "20m", "instance TTL hard cap per spawned callable's instance")
	deadlineMin := fs.Int("deadline-min", 15, "give up acquiring/waiting after N minutes")
	rates := fs.String("rates", "config/rates.json", "rate table path (unused by spawn-run today; accepted for CLI symmetry with real/session)")
	confirm := fs.Bool("i-understand-this-spends-money", false, "required: launches one billable instance per spawned callable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bucket == "" || *runID == "" || *ami == "" {
		return fmt.Errorf("usage: calque spawn-run --bucket B --run-id ID --ami AMI [--instance m7i.large] <script.py> --i-understand-this-spends-money")
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: calque spawn-run --bucket B --run-id ID --ami AMI <script.py> --i-understand-this-spends-money")
	}
	if !*confirm {
		return fmt.Errorf("refusing to launch: pass --i-understand-this-spends-money (launches one billable instance per .spawn()'d callable)")
	}
	o := spawnOpts{
		bucket: *bucket, region: *region, runID: *runID, instance: *instance, ami: *ami,
		ttl: *ttl, deadline: time.Duration(*deadlineMin) * time.Minute, ratesFP: *rates,
	}
	return spawnRunFromScript(o, fs.Arg(0))
}

// spawnRunFromScript parses script, extracts its .spawn() call sites, and
// drives spawnRun — the actual `calque spawn-run` CLI entry point wiring.
func spawnRunFromScript(o spawnOpts, script string) error {
	ctx := context.Background()
	rep := &leak.Report{}
	runner, runnerArgs := parse.DefaultRunner(pyastDir())

	app, err := parse.Parse(ctx, script, rep, runner, runnerArgs...)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	sites, err := parse.SpawnCallSitesReport(ctx, script, rep, runner, runnerArgs...)
	if err != nil {
		return fmt.Errorf("resolve spawn call sites: %w", err)
	}
	if len(rep.Leaks) > 0 {
		fmt.Println("--- leak report from parse (§10) ---")
		rep.Summary(os.Stdout)
	}
	return spawnRun(ctx, app, toSpawnCallSites(sites), o)
}

// toSpawnCallSites converts parse.SpawnCallSite (calque#112's addition to
// internal/parse, exposing what pyInvokeCall.Args already captured at parse
// time per calque#88) into calexec.SpawnCallSite — a trivial shape adapter
// so internal/exec doesn't need to import internal/parse just for one
// struct.
func toSpawnCallSites(sites []parse.SpawnCallSite) []calexec.SpawnCallSite {
	out := make([]calexec.SpawnCallSite, len(sites))
	for i, s := range sites {
		out[i] = calexec.SpawnCallSite{Target: s.Target, Args: s.Args}
	}
	return out
}
