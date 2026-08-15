package plan

import (
	"time"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// SpawnLauncher is calque's own launch-config BUILDER — the fields calque's
// callers already assemble per run, translated into a spawnaws.LaunchConfig
// for lagotto/pkg/snipe.Snipe to drive (calque#75/lagotto#106). It no longer
// owns a *spawnaws.Client or a Provision method: Snipe resolves its own
// region-pinned client internally (lagotto#111), so calque never needs to
// build or hold one just to acquire an instance.
//
// The command the instance runs is built by calque (spawn#351: there is no
// exported ECR/container primitive yet, tracked in spawn#353). We inject a
// docker-run bootstrap via LaunchConfig.JobArrayCommand and set a TTL so a
// runaway can't survive — spawn enforces a TTL floor regardless.
type SpawnLauncher struct {
	RunCmd     string // full command run on the instance (docker login/pull/run ...)
	TTL        string // e.g. "2h" — hard lifetime cap
	OnComplete string // "terminate" (default) so the instance dies when the job signals done
	Username   string // primary linux user (for pre-stop hook $HOME resolution)
	// AMI pins the machine image. Empty lets spawn auto-select (GetRecommendedAMI
	// -> GetAL2023AMI, resolving the "Deep Learning Base OSS Nvidia Driver GPU
	// AMI" SSM parameter for GPU instance types). spawn#356 (GPU AL2023 SSM
	// param missing g6e/g7/g7e auto-detect) was fixed upstream in spawn v0.79.0;
	// calque#75 verified this LIVE on all four target families (g6/g6e/g7/g7e):
	// auto-select resolves successfully, the instance boots, SSM comes online,
	// and — checked specifically on g7 — Docker + NVIDIA drivers + the nvidia
	// container runtime are all present and working (confirmed via
	// `docker info | grep nvidia` showing the nvidia runtime registered). Pin
	// this only when a specific AMI is required for a reason OTHER than the
	// old spawn#356 workaround (e.g. reproducibility, or an image lacking a
	// tool a bootstrap script needs).
	AMI string
	// IMDSv2HopLimit sets the instance metadata hop limit. Set to 2 when warmd
	// runs inside a docker container so it can reach instance-role creds via IMDS
	// (containers are one hop away; spawn's default of 1 blocks them).
	IMDSv2HopLimit int
	// RootVolumeGiB overrides the root EBS volume size. spawn defaults to 20 GiB,
	// far too small for the vLLM image (~15-20 GiB extracted) + model weights —
	// "no space left on device" during docker pull. Set ~200 for a GPU inference run.
	RootVolumeGiB int32
	// PricePerHour, if >0, is passed to spawn so it SKIPS its own per-launch
	// Pricing-API lookup. calque already gets R_a via truffle, so priming this
	// avoids ~1 redundant Pricing API call PER retry attempt during a capacity
	// wait (observed: hundreds over a long snipe).
	PricePerHour float64
	// Spot, if true, launches on the Spot market (spawn LaunchConfig.Spot). Spot
	// draws from a DIFFERENT capacity pool than on-demand — observed 2026-07-27:
	// g7e on-demand was exhausted region-wide (us-west-2, us-east-1) while g7e
	// spot landed instantly in us-east-1/eu-central-1/eu-west-2. Two honesty
	// consequences the caller must surface: (1) a spot box can be reclaimed
	// mid-run (interruption), and (2) the measured K's R_a is then a SPOT rate,
	// not on-demand — a different economic question than the headline K.
	Spot bool
	// SpotMaxPrice caps the bid in $/hr (spawn LaunchConfig.SpotMaxPrice). Empty
	// means "on-demand price as the cap" (spawn's default) — so a fill failure is
	// capacity, never "bid too low".
	SpotMaxPrice string
	// IamInstanceProfile is the IAM instance profile NAME (not ARN) attached
	// to the launched instance (calque#148) — without this, spawn's own
	// Launch never sets any instance profile at all (confirmed: it's a
	// pure passthrough, zero implicit default), so the instance has NO
	// credentials for the `aws s3 cp`/`aws s3 sync` calls its own bootstrap
	// script makes. Every caller MUST resolve one via
	// spawnaws.Client.CreateOrGetInstanceProfile before building this —
	// mirroring internal/pool's existing WorkerPolicy/FleetWorkerPolicy
	// pattern, the only place in this codebase that got this right before
	// calque#148. Empty reproduces the PRE-#148 (broken) behavior — kept
	// as a zero-value default only so existing tests that don't care about
	// IAM don't need updating, never as an intentional caller choice.
	IamInstanceProfile string
	// SecurityGroupIDs attaches one or more EC2 security groups to the
	// launched instance (calque#91 Workstream B) — e.g. the NFS ingress
	// group EnsureNFSSecurityGroup (efs.go) resolves for a script with a
	// real network_file_systems= mount. Empty (the default, every pre-
	// Workstream-B caller) reproduces prior behavior byte-for-byte: spawn's
	// own LaunchConfig.SecurityGroupIDs is nil, and spawn creates its own
	// default security group as it always has.
	SecurityGroupIDs []string
	// RunID and Command populate the calque:run-id/calque:command tags
	// (calque#166) — without these, an interrupted run leaves an instance
	// discoverable only by launch time/instance type (see
	// docs/guide/troubleshooting.md's pre-#166 workaround). Empty RunID
	// skips tagging entirely (some callers, e.g. spawn-run's per-callable
	// launches, may not have a single run-id concept yet).
	RunID   string
	Command string // "real", "ramp", "smoke", "spawn-run", ...
}

// Build translates SpawnLauncher's fields into a spawnaws.LaunchConfig for
// Acquirer.LaunchConfig. InstanceType/Region/AvailabilityZone/SubnetID are
// deliberately NOT set here — Acquirer/Snipe overrides those per attempt.
func (s SpawnLauncher) Build() spawnaws.LaunchConfig {
	onComplete := s.OnComplete
	if onComplete == "" {
		onComplete = "terminate"
	}
	ttl := s.TTL
	if ttl == "" {
		ttl = "2h"
	}
	return spawnaws.LaunchConfig{
		AMI:               s.AMI, // empty => spawn auto-selects (calque#75: verified live on g6/g6e/g7/g7e)
		TTL:               ttl,
		OnComplete:        onComplete,
		Username:          s.Username,
		JobArrayCommand:   s.RunCmd,
		RootVolumeSizeGiB: s.RootVolumeGiB,  // 0 => spawn default 20 GiB (too small for vLLM)
		PricePerHour:      s.PricePerHour,   // >0 => spawn skips its per-launch price lookup
		IMDSv2HopLimit:    s.IMDSv2HopLimit, // 2 for containers: warmd runs INSIDE docker and
		//                                     needs instance-role creds via IMDS, which is one
		//                                     network hop away — the default hop limit of 1 blocks it.
		Spot:               s.Spot,               // Spot market: different capacity pool than on-demand
		SpotMaxPrice:       s.SpotMaxPrice,       // "" => spawn caps at on-demand price
		IamInstanceProfile: s.IamInstanceProfile, // calque#148: empty => instance has NO S3/AWS credentials at all
		SecurityGroupIDs:   s.SecurityGroupIDs,   // calque#91 Workstream B: nil => spawn's own default SG (unchanged)
		Tags:               s.tags(),
	}
}

// tags builds the calque:* tag set (calque#166). Returns nil (not an empty
// map) when RunID is unset, so callers that don't yet have a run-id concept
// launch exactly as before — spawn's own buildTags() treats a nil user-tag
// map as "no additional tags", not an error.
func (s SpawnLauncher) tags() map[string]string {
	if s.RunID == "" {
		return nil
	}
	return map[string]string{
		"calque:run-id":     s.RunID,
		"calque:managed":    "true",
		"calque:command":    s.Command,
		"calque:created-at": time.Now().UTC().Format(time.RFC3339),
	}
}
