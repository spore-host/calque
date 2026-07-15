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
	// AMI pins the machine image. Empty lets spawn auto-select, but spawn's
	// GetRecommendedAMI resolves a GPU AL2023 SSM parameter that AWS does not
	// publish (ParameterNotFound) and misdetects g6e/g7/g7e as non-GPU — so for
	// GPU instances we pin a Deep Learning AMI explicitly. See spawn issue.
	AMI string
}

// Provision launches one instance of instanceType in region and returns the
// live handle fields calque needs. A capacity failure surfaces as a
// *spawnaws.LaunchError, which the Acquirer's classify() reads via smithy.
func (s *SpawnLauncher) Provision(ctx context.Context, instanceType, region string) (LaunchOutcome, error) {
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
		InstanceType:    instanceType,
		Region:          region,
		AMI:             s.AMI, // empty => spawn auto-selects (broken for GPU; pin for GPU)
		TTL:             ttl,
		OnComplete:      onComplete,
		Username:        s.Username,
		JobArrayCommand: s.RunCmd,
	}
	res, err := launcher.Provision(ctx, s.Client, cfg, launcher.Options{})
	if err != nil {
		return LaunchOutcome{}, err // *spawnaws.LaunchError; classified upstream
	}
	return LaunchOutcome{
		InstanceID:       res.InstanceID,
		Region:           region, // spawn#351: LaunchResult carries no Region; use ours
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
