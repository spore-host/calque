package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/cost"
	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/gpu"
	"github.com/spore-host/calque/internal/image"
	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/measure"
	"github.com/spore-host/calque/internal/plan"
	warm "github.com/spore-host/calque/worker/warm-runner"
)

// realOpts controls a real GPU inference run — the headline-K vehicle.
type realOpts struct {
	bucket       string
	region       string
	runID        string
	instance     string
	ami          string
	model        string // HF repo id, e.g. "Qwen/Qwen2.5-1.5B-Instruct"
	n            int    // number of prompts
	ttl          string
	deadline     time.Duration
	ratesFP      string
	spot         bool   // acquire on the Spot market (different capacity pool than on-demand)
	spotMaxPrice string // $/hr bid cap; "" => spawn caps at on-demand price
	// script, when set, is a Modal script to parse for its REAL .map()/
	// .starmap() iterable (calque#136) — opt-in; "" (the default) reproduces
	// today's synthesized-prompt behavior exactly, since realRun/fleetRun
	// don't otherwise parse any script at all.
	script string
	// secrets are name/value pairs from repeatable --secret NAME=VALUE
	// flags, injected into the runner's environment before @enter runs —
	// the generic counterpart to Modal's secrets=[...], which calque's
	// parser only ever recorded (leaked "recorded but NOT injected in the
	// spike") until this. nil/empty (the default) reproduces prior
	// behavior byte-for-byte: no env vars set beyond what the AMI/image
	// already provides.
	secrets map[string]string
	// itemFile, when set, replaces realOrSyntheticItems entirely with a
	// SINGLE item wrapping this file's raw bytes (calque real --item-file
	// PATH) — for a picked unit whose real signature takes `bytes`.
	// "" (the default) reproduces prior behavior byte-for-byte.
	itemFile string
	// entrypoint selects which @app.local_entrypoint() to drive when
	// --script has more than one (mirrors run --dry-run's own
	// --entrypoint, resolveEntrypoint) — previously real/ramp/fleetrun had
	// no way to specify this at all. "" (the default) is only valid for a
	// script with 0 or 1 entrypoints; ambiguous "" on 2+ becomes a leak +
	// synthesized-placeholder fallback, matching warmUnitForScript's
	// existing parse-failure fallback shape.
	entrypoint string
	// function, when set, drives this specific @app.function/@cls method by
	// NAME (calque real --function NAME) instead of pickWarmUnit's automatic
	// entrypoint/`.map()`-preference scan — takes priority over entrypoint's
	// own selection. Needed when the target callable isn't reachable through
	// any @app.local_entrypoint() at all (e.g. AI-Almanac's app.py: its only
	// entrypoint invokes the sibling run_benchmark, never
	// run_benchmark_local). "" (the default) reproduces prior behavior
	// byte-for-byte.
	function string
	// argFiles/argJSON (calque real --arg-file IDX=PATH / --arg-json
	// IDX=JSON) together build a REAL positional-args tuple for a picked
	// unit whose signature mixes a bytes arg with non-bytes ones (e.g.
	// run_benchmark_local(job_id: str, config: dict, input_bundle: bytes,
	// runtime_env: dict | None)) — itemFile's single-whole-payload-is-bytes
	// design can't express this. Every index from 0 up to the highest given
	// must be covered by exactly one of the two maps; mutually exclusive
	// with itemFile and with --n's synthesized/literal items. nil/empty
	// (the default) reproduces prior behavior byte-for-byte.
	argFiles map[int]string
	argJSON  map[int]string
	// pipPackages are third-party Python packages (calque real --pip
	// PACKAGE, repeatable) to install via uv on the instance before
	// running a --script-picked unit's REAL body — closes host mode's
	// previous "dependencies must already be on the AMI" gap (calque#148
	// follow-up) for scripts whose image chain wasn't statically
	// resolvable. nil/empty (the default) reproduces prior behavior
	// byte-for-byte.
	pipPackages []string
	// pythonVersion pins the interpreter uv installs (calque real
	// --python-version X.Y) — only meaningful alongside pipPackages.
	pythonVersion string
	// stageFiles are URL -> absolute-destination-path pairs (calque real
	// --stage-file URL=PATH, repeatable) downloaded via curl on the
	// instance before warmd runs — for a picked unit's body that shells
	// out to a hardcoded absolute path its original Docker image would
	// have placed there. nil/empty (the default) reproduces prior
	// behavior byte-for-byte.
	stageFiles map[string]string
	// allowCardSwap opts into target.CardSwapFor's curated substitution
	// table (calque real/ramp/fleetrun --allow-card-swap, calque#178) — a
	// CleanSwap gpu= site whose asked-for card has a VERIFIED entry gets
	// carried through as the swapped card instead, and instance selection
	// (recommendedTarget) resolves a real instance for that NEW card via
	// truffle instead of blindly keeping --instance's old hardcoded
	// default. false (the default) reproduces prior behavior byte-for-byte
	// — the asked-for card is always what gets used, exactly like today.
	allowCardSwap bool
}

// defaultRealInstance is calque real's pre-#178 hardcoded --instance
// default. main.go's flag default changed from this literal to "" (calque
// real/#178) so recommendedTarget can distinguish "operator didn't pass
// --instance at all" (may resolve via a swap) from "operator explicitly
// asked for this" (always wins). Applied here, AFTER swap resolution, so
// the no-swap case (the overwhelming majority of runs) stays exactly what
// it always was.
const defaultRealInstance = "g6.2xlarge"

// The real warm-unit bodies: actual vLLM. @enter loads the model ONCE; the
// method generates for one prompt. These mirror what map_batch_inference.py's
// @cls does — real B=1 inference, the swap-legal regime. warmd exec's them
// verbatim inside the vLLM container (runner.py wraps them as we tested).
const (
	realEnterBody = `import os
from vllm import LLM, SamplingParams
self.llm = LLM(model=os.environ["CALQUE_MODEL"], dtype="float16", gpu_memory_utilization=0.85, max_model_len=2048)
self.params = SamplingParams(temperature=0.7, max_tokens=128)`

	realMethodBody = `out = self.llm.generate([prompt], self.params)
return out[0].outputs[0].text`

	// realBatchMethodBody is the micro-batch shape (#68): `prompts` is the LIST of
	// payloads, passed to a SINGLE vLLM .generate(list) call — which is how vLLM
	// actually batches and fills the GPU (raising occupancy). It returns a LIST of
	// texts aligned 1:1 to inputs, as the batch protocol requires. Used only when
	// --batch-size > 1; the single-item body above stays the default.
	realBatchMethodBody = `outs = self.llm.generate(prompts, self.params)
return [o.outputs[0].text for o in outs]`
)

// realRun acquires a GPU, runs real vLLM inference over N prompts under the warm
// runner (model loaded once), collects results + the tach summary from S3, folds
// them into the cost model, and emits a verdict (§9). Deferred terminate so
// a mid-run failure never leaks the instance.
func realRun(o realOpts) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	instLabel := o.instance
	if instLabel == "" {
		instLabel = "(auto)" // resolved later — possibly via --allow-card-swap (calque#178)
	}
	fmt.Printf("=== calque REAL GPU run (model=%s N=%d region=%s instance=%s) ===\n", o.model, o.n, o.region, instLabel)

	// Route-away gate (§11, G3): refuse to rent a GPU for a model that's already an
	// exact Bedrock API call. Enforces the "--model must NOT be on Bedrock" contract
	// BEFORE any billable acquisition.
	if printOffersAndStop(bedrockOffersForModel(ctx, o.model, rep)) {
		return nil
	}

	warmdBin, err := buildWarmd(ctx)
	if err != nil {
		return fmt.Errorf("build warmd: %w", err)
	}
	fmt.Printf("[1/8] built warmd (linux/amd64)\n")

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.region))
	if err != nil {
		return err
	}
	s3c, err := calexec.NewS3ClientForBucket(ctx, o.bucket, o.region)
	if err != nil {
		return fmt.Errorf("s3 client for bucket %q: %w", o.bucket, err)
	}
	layout := calexec.NewLayout(o.bucket, o.runID)

	if err := calexec.UploadArtifacts(ctx, s3c, layout, warmdBin, "worker/warm-runner/runner.py", "worker/warm-runner/occupancy.py"); err != nil {
		return fmt.Errorf("upload artifacts: %w", err)
	}
	fmt.Printf("[2/8] uploaded artifacts\n")

	// N real prompts. The warm runner loads the model once (@enter) then drains
	// these. calque#136: when --script names a Modal script, use its REAL
	// .map()/.starmap() iterable when it's long enough; else (the default,
	// --script unset) this is byte-identical to the pre-existing synthesized
	// canned-sentence placeholder.
	if o.itemFile != "" && (len(o.argFiles) > 0 || len(o.argJSON) > 0) {
		return fmt.Errorf("--item-file and --arg-file/--arg-json are mutually exclusive")
	}
	app, unit, _ := warmUnitForScriptFn(ctx, o.script, o.entrypoint, o.function, rep)
	var items []warm.Item
	var base64ArgIndices []int
	forceStarmap := false
	switch {
	case len(o.argFiles) > 0 || len(o.argJSON) > 0:
		// calque real --arg-file IDX=PATH / --arg-json IDX=JSON: a REAL
		// positional-args tuple for a picked unit whose signature mixes a
		// bytes arg with non-bytes ones (e.g. run_benchmark_local(job_id:
		// str, config: dict, bundle: bytes, runtime_env: dict | None)) —
		// itemFile's single-whole-payload-is-bytes design can't express
		// this. The unit's own Invoke kind (likely not .starmap() at all —
		// this may be a plain function that just happens to take several
		// positional args) is irrelevant here: forceStarmap makes the
		// runner splat the tuple regardless of what static parsing detected.
		items, base64ArgIndices, err = itemFromArgs(o.argFiles, o.argJSON)
		if err != nil {
			return err
		}
		forceStarmap = true
	case o.itemFile != "":
		// calque real --item-file PATH: a REAL file's raw bytes as the
		// single item, for a picked unit whose signature takes `bytes`
		// (e.g. a netCDF/tarball bundle) — skips realOrSyntheticItems'
		// synthesized-placeholder path (and its calque#136 leak) entirely,
		// since this IS real data, just sourced from disk instead of a
		// script's own statically-literal .map() call.
		items, err = itemFromFile(o.itemFile)
		if err != nil {
			return err
		}
	default:
		items = realOrSyntheticItems(unit, o.n, func(i int) any {
			return fmt.Sprintf("In one sentence, summarize why fact #%d about scientific computing matters.", i)
		}, rep)
	}
	// calque#79 Part 1: when --script picked a REAL warm unit, ship ITS OWN
	// body (plus any .starmap()/.local() shape) instead of always driving the
	// hardcoded vLLM reference constants regardless of what the script does.
	// unset --script (the default) reproduces prior behavior byte-for-byte.
	hostMode := false
	registryRef := ""
	buildDockerfile := false
	buildTag := ""
	var dockerfileText string
	body := calexec.ManifestBody{EnterBody: realEnterBody, MethodBody: realMethodBody, MethodArg: "prompt"}
	if scriptBody, ok := manifestBodyForUnit(app, unit, rep); ok {
		if err := checkInvokeSupport(app.Script, unit.method, rep); err != nil {
			return err
		}
		// GPU guard parity with dry-run (run.go's swapLegal check, calque#7/§7):
		// a real launch must refuse the same flagged multi-GPU/coupled swaps
		// --dry-run does, rather than silently launching on a wrong-shaped
		// instance — this path had no GPU guard at all before calque#79.
		glog := gpu.RewriteApp(app, rep, o.allowCardSwap)
		if !swapLegal(glog, unit.class.Name) {
			return fmt.Errorf("gpu= swap for %q is FLAGGED (multi-GPU or coupled); out of single-node scope — see leak report", unit.class.Name)
		}
		body = scriptBody
		// calque#176/#177: prefer the method's own image (a method-level
		// image= override), falling back to the class's — mirrors
		// resolveCallableImage's own App->class->method fallback chain
		// (internal/parse/parse.go).
		img := unit.method.Image
		if img.Unresolved || img.Base == "" {
			img = unit.class.Image
		}
		bareRef, pullable := image.RegistryRef(img)
		switch {
		case image.NeedsBuild(img):
			// calque#177: the resolved chain has steps beyond a bare
			// pullable ref (or no pullable base at all) — build it ON THIS
			// INSTANCE from the script's OWN resolved .image chain instead
			// of falling back to a hand-typed --pip/--stage-file
			// substitute. internal/image.Render is the same, already
			// unit-tested Dockerfile renderer --dry-run already uses.
			df, rerr := image.Render(image.Spec{Image: img, WorkerDir: hostWorkerDir}, app.Script, rep)
			if rerr != nil {
				return fmt.Errorf("render Dockerfile for %s's resolved image: %w", unit.method.Name, rerr)
			}
			dockerfileText = df
			buildDockerfile = true
			buildTag = "calque-local:" + image.Digest(df)
			if pullable {
				// The Dockerfile's own FROM line may itself be a private
				// registry ref (e.g. a from_registry base with layered
				// .pip_install(...) on top) — docker build needs the same
				// pull-auth decision a plain pull would (bootstrap.go's
				// isECRHostname check), so still carry it through.
				registryRef = bareRef
			}
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"real run driving %s's OWN parsed body against a Dockerfile BUILT ON THE INSTANCE from its OWN resolved .image chain (calque#177) — any add_local_*/from_dockerfile step within that chain that calque can't stage is leaked separately (see internal/image)", unit.method.Name)
		case pullable:
			// calque#176: a bare pullable from_registry/from_aws_ecr ref
			// with nothing layered on top — pull it directly, no build
			// needed.
			registryRef = bareRef
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"real run driving %s's OWN parsed body against its OWN resolved image %q (calque#176)", unit.method.Name, registryRef)
		default:
			hostMode = true // no resolved image at all — a parsed script's own body is typically not a vLLM/docker workload
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"real run driving %s's OWN parsed body (calque#79), host-mode (no docker/vLLM image pull) — if the script needs non-stdlib dependencies, they must already be on the AMI", unit.method.Name)
		}
	}
	if forceStarmap {
		// --arg-file/--arg-json: the picked unit's real signature is a
		// tuple of positional args regardless of what static .starmap()
		// detection found (it may not be .starmap()'d at all — just a
		// plain multi-arg function like run_benchmark_local) — force the
		// runner to splat it. body.MethodArgs was already set from the
		// unit's own real parameter list (nonSelfArgs) by
		// manifestBodyForUnit above.
		body.Starmap = true
	}
	body.Secrets = o.secrets
	body.PayloadIsBase64Bytes = o.itemFile != ""
	body.Base64ArgIndices = base64ArgIndices
	// calque real --secret NAME=VALUE closes the gap the parser's own
	// static leak ("secrets recorded but NOT injected in the spike",
	// internal/parse/parse.go) flags — but only for the names the caller
	// actually passed. Leak specifically which of the script's OWN
	// declared secret names (unit.class.Config.Secrets) were never
	// covered by a --secret flag, so a run that's still missing one fails
	// loudly with a clear cause instead of the payload's own bare
	// KeyError/NameError being the only signal.
	if unit.class.Config.Secrets != nil {
		var missing []string
		for _, name := range unit.class.Config.Secrets {
			if _, ok := o.secrets[name]; !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			rep.Addf(leak.PrimEnter, leak.KindSemanticGap, app.Script, unit.method.Line,
				"secret(s) %v declared but not covered by --secret; the payload will fail if it reads them", missing)
		}
	}
	// calque#79's plumbing for staging/committing a script's REAL
	// modal.Volume.from_name(...) mounts (plan.ResolveVolumes,
	// internal/exec's VolumeSync/VolumeCommit + warmd's aws-s3-sync
	// stage/commit) was complete but never called from any real writer —
	// every one passed nil, nil here. No new flag: ResolveVolumes derives
	// mount path + S3 prefix entirely from the picked unit's own
	// volumes= kwarg, already in the IR. A script with no Volumes (the
	// vast majority, and every corpus script driven through this path
	// before calque#79) gets an empty slice both ways — byte-for-byte
	// unchanged behavior.
	volumeSync, volumeCommit := volumeSpecsForApp(app, o.bucket, rep)
	// calque#91 Workstream A: a script's REAL modal.CloudBucketMount(...)
	// mounts (its OWN S3 bucket, mounted live via mountpoint-s3 — NOT
	// calque's --bucket staging area the way an ordinary Volume is) resolve
	// into shell lines spliced into the bootstrap script, plus the distinct
	// bucket names the instance's IAM role needs read/write/list access to.
	// A script with no CloudBucketMounts (the vast majority) gets an empty
	// slice both ways — byte-for-byte unchanged behavior.
	cloudBucketMountLines, cloudBucketMountBuckets := cloudBucketMountSpecsForApp(app, rep)
	// calque#148, widened: bootstrap.go's host-mode branch ALWAYS
	// provisions a uv-managed venv now (not just when --pip supplies real
	// deps), so warmd must ALWAYS invoke that SAME venv's interpreter for
	// a host-mode run — the manifest's PythonBin and the bootstrap's venv
	// path MUST derive from the same hostWorkerDir, or warmd falls back
	// to a bare "python3" outside the venv entirely. Docker-mode runs
	// don't create this venv at all (the container has its own Python),
	// so PythonBin stays unset there — unchanged from before.
	if hostMode {
		body.PythonBin = hostWorkerDir + "/.venv/bin/python3"
	}
	if err := calexec.WriteManifestBody(ctx, s3c, layout, body, hostWorkerDir, items, volumeSync, volumeCommit); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if buildDockerfile {
		// calque#177: upload the rendered Dockerfile alongside warmd/runner.py —
		// the instance's existing `aws s3 cp --recursive` artifact sync (every
		// docker-mode run already does this) downloads it for free.
		if err := calexec.UploadDockerfile(ctx, s3c, layout, dockerfileText); err != nil {
			return fmt.Errorf("upload Dockerfile: %w", err)
		}
	}
	if hostMode {
		fmt.Printf("[3/8] wrote manifest (%d items, %s's own @enter+@method)\n", o.n, unit.method.Name)
	} else {
		fmt.Printf("[3/8] wrote manifest (%d real prompts, vLLM @enter+@method)\n", o.n)
	}

	// Docker-mode bootstrap (the default, unchanged): pull vLLM, run --gpus all,
	// pass the model via env, warmd drives the warm runner inside the container.
	// calque#79 Part 1: a --script-picked real unit runs HOST-MODE instead — its
	// own body is typically a plain CPU/GPU function, not necessarily a vLLM
	// inference call, mirroring spawnrun.go's runSpawnShard precedent.
	boot := calexec.BootstrapConfig{
		BaseImage: "vllm/vllm-openai:latest", Bucket: o.bucket, ArtifactPrefix: layout.ArtifactPfx,
		ManifestKey: layout.ManifestKey, WorkerDir: hostWorkerDir, Region: o.region,
		LogKey: layout.LogKey, HostMode: hostMode, ModelEnv: o.model,
		PipPackages: o.pipPackages, PythonVersion: o.pythonVersion,
		StageFiles: o.stageFiles, RegistryRef: registryRef,
		BuildDockerfile: buildDockerfile, BuildTag: buildTag,
		CloudBucketMountLines: cloudBucketMountLines,
	}

	// calque#134/#178: when --script named a real parsed unit, carry its
	// actual requested card through Recommend instead of hardcoding
	// DefaultCard; when --allow-card-swap applied a verified substitution
	// (and the operator didn't pin --instance explicitly), resolve a real
	// instance for the NEW card via truffle instead of the old
	// hardcoded-default fallback sized for the OLD card. Computed here
	// (before pricing/AZ-sweep/acquire) so every one of those steps below
	// uses the SAME possibly-swapped instance, not the stale o.instance.
	tgt := recommendedTarget(unit, o.instance, defaultRealInstance, o.allowCardSwap, rep)
	inst := tgt.Instance

	// Price once via truffle (also R_a).
	var pricePerHr float64
	if pricer, perr := plan.NewTrufflePricer(ctx); perr == nil {
		if rate, rerr := pricer.OnDemandPrice(ctx, inst, o.region); rerr == nil {
			pricePerHr = rate
			fmt.Printf("      priced %s @ %s = $%.4f/hr (truffle)\n", inst, o.region, rate)
		}
	}

	// AZ sweep (offered AZs w/ default subnet).
	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), inst); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	// calque#91 Workstream B: a script's REAL modal.NetworkFileSystem.
	// from_name(...) mounts (a pre-provisioned, bring-your-own EFS
	// filesystem — NOT auto-created) resolve into shell lines spliced into
	// the bootstrap script, a security group ID the launched instance needs
	// attached (NFS/2049 ingress), and a NARROWED places list (only AZs
	// with a live mount target for every required mount). Must run BEFORE
	// the Acquirer is built, since it can genuinely narrow (or, if no AZ
	// has coverage, error out) the placements the acquirer sweeps. A script
	// with no NetworkFileSystems (the vast majority) leaves places
	// unchanged and returns no lines/security groups.
	nfsMountLines, nfsSecurityGroupIDs, narrowedPlaces, err := networkFileSystemSpecsForApp(ctx, efs.NewFromConfig(cfg), ec2.NewFromConfig(cfg), app, o.region, places, rep)
	if err != nil {
		return fmt.Errorf("resolve network_file_systems: %w", err)
	}
	places = narrowedPlaces
	// boot was already constructed above (before this AZ-narrowing step, to
	// keep boot's own field-init block a single literal); splice the
	// resolved NFS mount lines in now — nil/empty (the default) is a no-op.
	boot.NFSMountLines = nfsMountLines

	spawnClient, err := spawnaws.NewClientWithRegion(ctx, o.region)
	if err != nil {
		return fmt.Errorf("spawn client: %w", err)
	}
	// calque#148: without a real IAM instance profile, the launched
	// instance has NO credentials for the `aws s3 cp`/`aws s3 sync` calls
	// its own bootstrap script makes — not even for uploading its OWN
	// bootstrap log on failure, which is why a bootstrap failure on this
	// path was previously totally silent (no log, no error, just a
	// timeout at the deadline). Scoped to just this run's own bucket, plus
	// (calque#91 Workstream A) any distinct bucket(s) the script's own
	// resolved CloudBucketMount(s) reference.
	iamProfile, err := plan.RealRunInstanceProfile(ctx, spawnClient, o.region, o.bucket, cloudBucketMountBuckets...)
	if err != nil {
		return fmt.Errorf("set up IAM instance profile: %w", err)
	}
	launchCfg := plan.SpawnLauncher{
		RunCmd: boot.Command(), TTL: o.ttl, OnComplete: "terminate",
		Username: "ubuntu", AMI: o.ami, PricePerHour: pricePerHr,
		IMDSv2HopLimit: 2,   // warmd runs INSIDE docker; needs IMDS creds one hop away
		RootVolumeGiB:  200, // vLLM image + weights blow past spawn's 20 GiB default
		Spot:           o.spot, SpotMaxPrice: o.spotMaxPrice,
		IamInstanceProfile: iamProfile,
		SecurityGroupIDs:   nfsSecurityGroupIDs, // calque#91 Workstream B: nil unless the script has a real network_file_systems= mount
		RunID:              o.runID, Command: "real",
	}.Build()
	if o.spot {
		// Honesty (§9/§10): a spot run measures K against a SPOT R_a, and the box
		// can be reclaimed mid-run. Say so loudly and leak it, so the resulting K
		// is never read as the on-demand headline number.
		bidCap := o.spotMaxPrice
		if bidCap == "" {
			bidCap = "on-demand price"
		}
		fmt.Printf("[spot] acquiring on the SPOT market (max bid %s). NOTE: interruptible mid-run; "+
			"any cost verdict measured here is against a SPOT rate, not the on-demand one.\n", bidCap)
		rep.Addf(leak.PrimAcquire, leak.KindSemanticGap, "real", 0,
			"spot acquisition: R_a is a spot rate and the instance is interruptible — this is a spot-rate cost measurement, not the on-demand one")
	}
	acq := &plan.Acquirer{
		LaunchConfig: launchCfg, Report: rep, Deadline: o.deadline, Placements: places,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			fmt.Printf("      ...swept %d attempt(s), no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	fmt.Printf("[4/8] acquiring %s in %s (block-and-wait, AZ-sweep)...\n", inst, o.region)
	acquired, err := acq.Acquire(ctx, tgt, o.region)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	fmt.Printf("      acquired %s (%s) after %s\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))

	defer func() {
		fmt.Printf("[8/8] terminating %s ...\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (TTL %s will reap)\n", acquired.InstanceID, tErr, o.ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		} else {
			fmt.Printf("      terminated %s\n", acquired.InstanceID)
		}
	}()

	// Wait for warmd's summary. Real vLLM model load + N generations takes minutes
	// (model download + load), so allow a generous window bounded by the deadline.
	// spawn#497 (spored v0.100.0+): also check the instance's spawn:last-heartbeat
	// tag, so a genuinely hung/gone instance fails fast instead of dead-waiting the
	// whole deadline — the exact ambiguity ("still legitimately running" vs.
	// "stuck") a real N=100 fleet re-verification run hit (calque#141/#142/#143).
	fmt.Printf("[5/8] waiting for warmd summary (vLLM load + %d generations)...\n", o.n)
	summaryBytes, err := calexec.WaitForSummaryLiveness(ctx, s3c, ec2.NewFromConfig(cfg), acquired.InstanceID, layout, o.deadline, 15*time.Second, staleHeartbeatAfter,
		func(elapsed time.Duration) { fmt.Printf("      ...running (%s)\n", elapsed.Round(time.Second)) })
	if err != nil {
		var stale *calexec.ErrInstanceStale
		if errors.As(err, &stale) {
			return fmt.Errorf("instance went unresponsive mid-run (%w) — its spawn:last-heartbeat tag stopped advancing; not a work-in-progress timeout", stale)
		}
		// Fast-failure: the bootstrap exited without a summary — its log tells us why.
		var bf *calexec.ErrBootstrapFailed
		if errors.As(err, &bf) {
			fmt.Fprintf(os.Stderr, "BOOTSTRAP FAILED (fast-detected) — log tail:\n%s\n", tail([]byte(bf.BootstrapLog), 2500))
			return fmt.Errorf("bootstrap failed on the instance (see log above)")
		}
		if logBytes, lerr := getS3(ctx, s3c, o.bucket, layout.LogKey); lerr == nil {
			fmt.Fprintf(os.Stderr, "--- bootstrap.log (tail) ---\n%s\n", tail(logBytes, 2500))
		}
		return fmt.Errorf("wait for summary: %w", err)
	}
	var summary struct {
		EnterSeconds float64              `json:"enter_seconds"`
		EnterCount   int                  `json:"enter_count"`
		PerItemSecs  []float64            `json:"per_item_secs"`
		Failed       []int                `json:"failed"`
		Occupancy    calexec.OccupancyRaw `json:"occupancy"`
	}
	_ = json.Unmarshal(summaryBytes, &summary)
	fmt.Printf("[6/8] summary: @enter x%d (%.1fs load), %d items, %d failed, occupancy %s\n",
		summary.EnterCount, summary.EnterSeconds, len(summary.PerItemSecs), len(summary.Failed), occStr(summary.Occupancy))

	results, missing, err := calexec.Collect(ctx, s3c, layout.Bucket, layout.ResultPrefix, len(items))
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	fmt.Printf("[7/8] collected %d/%d results (%d missing)\n", len(results), len(items), len(missing))
	if len(results) > 0 {
		fmt.Printf("      sample result[0]: %.120q\n", fmt.Sprint(results[0].Result))
	}

	// Fold measured ground truth into the cost model and emit K.
	if err := emitK(o, inst, summary.PerItemSecs, summary.EnterSeconds, summary.Occupancy, acquired, pricePerHr); err != nil {
		return err
	}

	fmt.Println("\n--- leak report (§10) ---")
	rep.Summary(os.Stdout)
	return nil
}

// volumeSpecsForApp resolves app's REAL modal.Volume.from_name(...) mounts
// (calque#79) into the sync-before-@enter and commit-after-@method-drains
// spec lists a real-AWS run's manifest carries — plan.ResolveVolumes,
// internal/exec's VolumeSync/VolumeCommit, and warmd's own aws-s3-sync
// stage/commit plumbing were all already built and tested, just never
// called from realRun/fleetRun before this. Factored out as a pure
// function (no ctx/S3) so the var-name/Modal-name reversal below is
// unit-testable without a real script/S3 client.
//
// CommittedVolumes is keyed by the module-level Volume VAR name (e.g.
// "forecast_volume"), but VolumeMount.Name is the Modal volume NAME
// (from_name's argument) — reverse app.Volumes (var -> Modal name) once to
// check commit status per resolved mount. A script with no Volumes (the
// vast majority) returns two nil slices — byte-for-byte the same as the
// hardcoded nil, nil this replaces.
func volumeSpecsForApp(app ir.App, bucket string, rep *leak.Report) (sync, commit []calexec.VolumeSyncSpec) {
	varNameByModalName := make(map[string]string, len(app.Volumes))
	for varName, modalName := range app.Volumes {
		varNameByModalName[modalName] = varName
	}
	for _, m := range plan.ResolveVolumes(app, rep) {
		spec := calexec.VolumeSyncSpec{URI: m.URI(bucket), MountPath: m.MountPath}
		sync = append(sync, spec)
		if app.CommittedVolumes[varNameByModalName[m.Name]] {
			commit = append(commit, spec)
		}
	}
	return sync, commit
}

// networkFileSystemSpecsForApp resolves app's REAL modal.NetworkFileSystem.
// from_name(...) mounts (calque#91 Workstream B) into the already-rendered
// shell lines spliced into BootstrapConfig.NFSMountLines, the security
// group ID(s) the launched instance needs attached (for NFS/2049 ingress),
// and a NARROWED placement list — unlike cloudBucketMountSpecsForApp, this
// one genuinely makes AWS calls (DiscoverEFSFilesystem/
// ResolveMountTargetsForAZs/EnsureNFSSecurityGroup, all real
// DescribeFileSystems/DescribeMountTargets/DescribeSecurityGroups/
// CreateSecurityGroup/AuthorizeSecurityGroupIngress round-trips), so it
// takes ctx + AWS clients and can genuinely error (a missing/ambiguous EFS
// filesystem tag, or an AZ sweep with NO coverage at all for a required
// mount, is a hard failure — not a leak: there is no placement the acquirer
// could land in that would make the mount work). places is the AZ-sweep
// result BEFORE narrowing (calexec.AZsForInstance's own output, translated
// to plan.Placement); the returned narrowed slice is the intersection of
// places with "has a live EFS mount target for EVERY required
// NetworkFileSystem" — narrowing to zero when the app DOES declare
// network_file_systems is treated as a hard error (no AZ the acquirer could
// land in would let the mount succeed), never a silent full-open sweep. A
// script with no NetworkFileSystems (the vast majority) returns (nil, nil,
// places, nil) — the SAME places slice the caller already had, unchanged.
func networkFileSystemSpecsForApp(ctx context.Context, efsClient *efs.Client, ec2Client *ec2.Client, app ir.App, region string, places []plan.Placement, rep *leak.Report) (lines []string, securityGroupIDs []string, narrowed []plan.Placement, err error) {
	mounts := plan.ResolveNetworkFileSystems(app, rep)
	if len(mounts) == 0 {
		return nil, nil, places, nil
	}

	azs := make([]string, 0, len(places))
	seenAZ := map[string]bool{}
	for _, p := range places {
		if !seenAZ[p.AZ] {
			seenAZ[p.AZ] = true
			azs = append(azs, p.AZ)
		}
	}

	// dnsByName is keyed by Modal NetworkFileSystem name (distinct mounts can
	// resolve to distinct EFS filesystems, each with its own DNS name); az
	// coverage is the INTERSECTION across every distinct filesystem — an AZ
	// only counts if EVERY required mount has a live mount target there.
	dnsByName := map[string]string{}
	var azCoverage map[string]bool
	seenName := map[string]bool{}
	for _, m := range mounts {
		if seenName[m.Name] {
			continue
		}
		seenName[m.Name] = true

		fsID, derr := plan.DiscoverEFSFilesystem(ctx, efsClient, m.Name)
		if derr != nil {
			return nil, nil, nil, derr
		}
		dnsByName[m.Name] = spawnaws.GetEFSDNSName(fsID, region)

		dnsPerAZ, missingAZs, rerr := plan.ResolveMountTargetsForAZs(ctx, efsClient, fsID, region, azs)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		if len(missingAZs) > 0 {
			rep.Addf(leak.PrimVolume, leak.KindIntegrationEdge, app.Script, 0,
				"NetworkFileSystem %q (EFS filesystem %s): no mount target in AZ(s) %v — placement narrowed away from them", m.Name, fsID, missingAZs)
		}
		thisCoverage := map[string]bool{}
		for az := range dnsPerAZ {
			thisCoverage[az] = true
		}
		if azCoverage == nil {
			azCoverage = thisCoverage
		} else {
			for az := range azCoverage {
				if !thisCoverage[az] {
					delete(azCoverage, az)
				}
			}
		}
	}

	for _, p := range places {
		if azCoverage[p.AZ] {
			narrowed = append(narrowed, p)
		}
	}
	if len(narrowed) == 0 {
		return nil, nil, nil, fmt.Errorf("network_file_systems declared, but no offered AZ has a live EFS mount target for every required mount — no placement the acquirer could land in would let the mount succeed")
	}

	vpcID, verr := plan.DefaultVPCID(ctx, ec2Client)
	if verr != nil {
		return nil, nil, nil, fmt.Errorf("resolve default VPC for NFS security group: %w", verr)
	}
	sgID, serr := plan.EnsureNFSSecurityGroup(ctx, ec2Client, vpcID)
	if serr != nil {
		return nil, nil, nil, fmt.Errorf("ensure NFS security group: %w", serr)
	}

	lines = plan.NFSMountCommands(mounts, dnsByName)
	return lines, []string{sgID}, narrowed, nil
}

// cloudBucketMountSpecsForApp resolves app's REAL modal.CloudBucketMount(...)
// mounts (calque#91 Workstream A) into the already-rendered shell lines
// spliced into BootstrapConfig.CloudBucketMountLines, plus the distinct S3
// bucket names (the SCRIPT'S OWN buckets, not calque's --bucket staging
// area) the instance's IAM role needs read/write/list access to — mirrors
// volumeSpecsForApp's factoring (a pure function, no ctx/S3, so it's
// unit-testable without a real script/S3 client). A script with no
// CloudBucketMounts (the vast majority) returns (nil, nil) — byte-for-byte
// the same as the hardcoded nil, nil this replaces.
func cloudBucketMountSpecsForApp(app ir.App, rep *leak.Report) (lines []string, buckets []string) {
	mounts := plan.ResolveCloudBucketMounts(app, rep)
	if len(mounts) == 0 {
		return nil, nil
	}
	lines = plan.MountCommands(mounts)
	seen := map[string]bool{}
	for _, m := range mounts {
		if !seen[m.BucketName] {
			seen[m.BucketName] = true
			buckets = append(buckets, m.BucketName)
		}
	}
	return lines, buckets
}

func emitK(o realOpts, inst string, perItem []float64, enterSec float64, occ calexec.OccupancyRaw, acq plan.Acquired, priceHr float64) error {
	rates, err := cost.LoadRates(o.ratesFP)
	if err != nil {
		return fmt.Errorf("rates: %w", err)
	}
	pi := measure.Aggregate(perItem)
	m := measure.Measurement{
		CardAskedFor: "H100", // map_batch asked for H100; R_m uses that (asymmetry §9)
		InstanceUsed: inst,   // the ACTUAL instance used — may differ from o.instance when --allow-card-swap resolved a new one (calque#178)
		PerItem:      pi,
		Occupancy: measure.OccupancySummary{
			MeanOccupancy: occ.MeanOccupancy, Samples: occ.Samples, Source: occ.Source,
			Measured: occ.Measured, Scope: occ.ScopeOrWholeRun(),
		},
		AcquiredAt: acq.AcquiredAt, TerminatedAt: time.Now(), EnterSeconds: enterSec,
		AcquireWaitSeconds: acq.TimeToAcquire().Seconds(),
	}
	occFrac, occMeasured := m.OccupancyFraction()
	_, awsMeasured, _ := rates.AWSOnDemandPerSecond(inst)
	model := &cost.Model{Rates: rates, M: cost.Measured{
		CardAskedFor: m.CardAskedFor, InstanceUsed: m.InstanceUsed, SecPerItem: pi.MeanSecs,
		Occupancy: occFrac, SampleItems: pi.Count, AWSRateMeasured: awsMeasured,
		AcquireSeconds: m.AcquireWaitSeconds, EnterSeconds: enterSec,
		OccupancyScope: m.Occupancy.ScopeOrWholeRun(),
	}}
	fmt.Println("\n--- cost model (§9) — MEASURED on real GPU ---")
	verdict, err := model.Verdict(100000)
	switch {
	case err == cost.ErrNoComputeMeasured:
		fmt.Println("K undefined: per-item compute ~0 (unexpected for real inference — check results).")
	case err != nil:
		return fmt.Errorf("cost: %w", err)
	default:
		fmt.Print(verdict)
	}
	if !occMeasured {
		fmt.Println("NOTE: occupancy unmeasured (nvidia-smi found no samples) — K's occupancy input is a proxy.")
	} else {
		fmt.Printf("This K is grounded in a REAL measured run: %d items @ %.3fs/item, %.0f%% occupancy on %s.\n",
			pi.Count, pi.MeanSecs, occFrac*100, inst)
		fmt.Printf("  %s\n", calexec.OccScopeNote(occ))
	}
	return nil
}

func occStr(o calexec.OccupancyRaw) string {
	if !o.Measured || o.MeanOccupancy == nil {
		return fmt.Sprintf("unmeasured (%s)", o.Source)
	}
	// Show the primary + every metric collected, so nvidia-smi's coarse
	// utilization.gpu can be compared against DCGM SM-activity (§8, Scott's note).
	s := fmt.Sprintf("%.0f%% [primary=%s]", *o.MeanOccupancy*100, o.OccupancySource)
	if len(o.Metrics) > 0 {
		parts := ""
		for _, k := range []string{"dcgm_sm", "nvsmi_sm", "nvsmi_util"} {
			if v, ok := o.Metrics[k]; ok && v != nil {
				parts += fmt.Sprintf(" %s=%.0f%%", k, *v*100)
			}
		}
		s += " {" + parts + " }"
	}
	return s
}

func getS3(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, error) {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	b := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := out.Body.Read(tmp)
		b = append(b, tmp[:n]...)
		if e != nil {
			break
		}
	}
	return b, nil
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}

// oneLine collapses a multi-line/verbose AWS error to a single trimmed log line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}
