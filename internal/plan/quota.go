// Pre-flight quota check for the fleet launcher (calque#141). A real N=100k
// fleet run (calque#18, 2026-08-09/10) found out about an account's real
// Spot ceiling (64 vCPUs = 8 concurrent g7e.2xlarge) only via a live
// MaxSpotInstanceCountExceeded error, after already committing to launching
// 10 shards in parallel. QuotaCeiling lets a caller ask "how many concurrent
// acquisitions of this instance type/region/market can this account actually
// sustain right now?" BEFORE committing to a shard count, mirroring spawn's
// own resolveAutoMaxConcurrent (spawn#492,
// cmd/launch_sweep_quota.go:deriveMaxConcurrentFromCombos) — same libraries,
// same headroom-over-vCPUs arithmetic, just calque's own copy since calque
// calls truffle as a Go library directly rather than shelling out to spawn's
// CLI.
package plan

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	truffleaws "github.com/spore-host/truffle/pkg/aws"
	truffleQuotas "github.com/spore-host/truffle/pkg/quotas"
)

// quotaGetter is the slice of truffle's *quotas.Client this package depends
// on. Kept as an interface (mirroring Resolver/Pricer in truffle.go) so
// QuotaCeiling is testable offline with a fake, instead of requiring live AWS
// credentials/API calls the way truffle's own *quotas.Client always does.
type quotaGetter interface {
	GetQuotas(ctx context.Context, region string) (*truffleQuotas.QuotaInfo, error)
}

// capsGetter is the slice of truffle's *aws.Client this package depends on:
// resolving an instance type's real vCPU count, so a per-family vCPU quota
// headroom can be converted into an instance count without re-parsing the
// type's size suffix (the same unreliable-for-nonlinear-sizes guessing
// truffle's own quotas.getVCPUCount flags as a last-resort fallback).
type capsGetter interface {
	GetCapabilities(ctx context.Context, instanceType, region string) (*truffleaws.Capabilities, error)
}

// QuotaCeiling returns how many concurrent instanceType acquisitions the
// account's real quota headroom in region can sustain right now (calque#141).
//
// It queries truffle's quota client for the instance type's quota family
// (truffleQuotas.GetQuotaFamily), reads the Spot or On-Demand quota/usage
// pair depending on spot, computes headroom = quota - usage, and divides by
// the instance type's real vCPU count (via truffle's Capabilities.VCPUs,
// from a live DescribeInstanceTypes call — not a guessed size-suffix) to get
// an instance-count ceiling.
//
// cfg is the caller's already-region-loaded AWS config; QuotaCeiling builds
// its own truffle clients from it rather than requiring the caller to
// construct them, mirroring spawn's deriveMaxConcurrentFromCombos.
func QuotaCeiling(ctx context.Context, cfg aws.Config, instanceType, region string, spot bool) (int, error) {
	q := truffleQuotas.NewClientFromConfig(cfg)
	c := truffleaws.NewClientFromConfig(cfg)
	return quotaCeiling(ctx, q, c, instanceType, region, spot)
}

// quotaCeiling is QuotaCeiling's testable core: given a quotaGetter and
// capsGetter (real truffle clients in production, fakes in tests), compute
// the headroom-derived instance-count ceiling.
func quotaCeiling(ctx context.Context, q quotaGetter, c capsGetter, instanceType, region string, spot bool) (int, error) {
	info, err := q.GetQuotas(ctx, region)
	if err != nil {
		return 0, fmt.Errorf("quota ceiling: quota lookup for %s in %s: %w", instanceType, region, err)
	}

	family := truffleQuotas.GetQuotaFamily(instanceType)
	var quota, usage int32
	if spot {
		quota, usage = info.Spot[family], info.SpotUsage[family]
	} else {
		quota, usage = info.OnDemand[family], info.Usage[family]
	}
	headroom := quota - usage
	if headroom < 0 {
		headroom = 0 // already over quota (e.g. someone else's concurrent usage) — 0 concurrent fits, not negative
	}

	caps, err := c.GetCapabilities(ctx, instanceType, region)
	if err != nil {
		return 0, fmt.Errorf("quota ceiling: capabilities for %s in %s: %w", instanceType, region, err)
	}
	if !caps.Found || caps.VCPUs <= 0 {
		return 0, fmt.Errorf("quota ceiling: vCPU count unavailable for %s in %s (cannot convert quota headroom to an instance count)", instanceType, region)
	}

	return int(headroom / caps.VCPUs), nil
}
