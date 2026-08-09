package plan

import (
	"context"
	"time"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// SpawnLauncher is the real Launcher, wrapping spawn.launcher.Provision — the
// acquire+bring-up primitive confirmed in spawn#351 (spawn owns RunInstances).
//
// The command the instance runs is built by calque (spawn#351: there is no
// exported ECR/container primitive yet, tracked in spawn#353). We inject a
// docker-run bootstrap via LaunchConfig.JobArrayCommand and set a TTL so a
// runaway can't survive — spawn enforces a TTL floor regardless.
type SpawnLauncher struct {
	Client     *spawnaws.Client
	Image      string        // ECR image ref to run
	RunCmd     string        // full command run on the instance (docker login/pull/run ...)
	TTL        string        // e.g. "2h" — hard lifetime cap
	OnComplete string        // "terminate" (default) so the instance dies when the job signals done
	Username   string        // primary linux user (for pre-stop hook $HOME resolution)
	Timeout    time.Duration // per-Provision call timeout
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
}

// Provision launches one instance of instanceType in region and returns the
// live handle fields calque needs. A capacity failure surfaces as a
// *spawnaws.LaunchError, which the Acquirer's classify() reads via smithy.
func (s *SpawnLauncher) Provision(ctx context.Context, instanceType, region, az, subnet string) (LaunchOutcome, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	onComplete := s.OnComplete
	if onComplete == "" {
		onComplete = "terminate"
	}
	ttl := s.TTL
	if ttl == "" {
		ttl = "2h"
	}
	cfg := spawnaws.LaunchConfig{
		InstanceType:      instanceType,
		Region:            region,
		AvailabilityZone:  az,     // "" => EC2 chooses; set by the Acquirer's AZ sweep
		SubnetID:          subnet, // default subnet for the AZ; avoids InvalidInput in AZs w/o one
		AMI:               s.AMI,  // empty => spawn auto-selects (broken for GPU; pin for GPU)
		TTL:               ttl,
		OnComplete:        onComplete,
		Username:          s.Username,
		JobArrayCommand:   s.RunCmd,
		RootVolumeSizeGiB: s.RootVolumeGiB,  // 0 => spawn default 20 GiB (too small for vLLM)
		PricePerHour:      s.PricePerHour,   // >0 => spawn skips its per-launch price lookup
		IMDSv2HopLimit:    s.IMDSv2HopLimit, // 2 for containers: warmd runs INSIDE docker and
		//                                     needs instance-role creds via IMDS, which is one
		//                                     network hop away — the default hop limit of 1 blocks it.
		Spot:         s.Spot,         // Spot market: different capacity pool than on-demand
		SpotMaxPrice: s.SpotMaxPrice, // "" => spawn caps at on-demand price
	}
	res, err := launcher.Provision(ctx, s.Client, cfg, launcher.Options{})
	if err != nil {
		return LaunchOutcome{}, err // *spawnaws.LaunchError; classified upstream
	}
	return LaunchOutcome{
		InstanceID:       res.InstanceID,
		Region:           region, // res.Region also available since spawn#352; ours is authoritative here
		AvailabilityZone: res.AvailabilityZone,
		PublicIP:         res.PublicIP,
		State:            res.State,
	}, nil
}

// NewSpawnClient builds a region-pinned spawn client (spawn#351: use
// NewClientWithRegion so AMI/AZ/RunInstances resolve consistently).
func NewSpawnClient(ctx context.Context, region string) (*spawnaws.Client, error) {
	return spawnaws.NewClientWithRegion(ctx, region)
}
