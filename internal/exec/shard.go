package exec

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	warm "github.com/spore-host/calque/worker/warm-runner"
)

// Multi-instance .map fan-out (spec §15, Gap D). We shard N items across S
// single-node instances, run each shard's manifest independently, then collect the
// UNION back into one globally-ordered result set with one global missing[]. This
// is embarrassingly-parallel fan-out across independent boxes — NOT §1's forbidden
// multi-node/gang scheduling; no instance depends on another mid-run.

// Shard is one instance's slice of the work: a contiguous index range with its own
// manifest + result prefix. The GLOBAL item index is preserved on every warm.Item,
// so Collect (which keys by Result.Index) orders the union correctly regardless of
// which shard produced which item.
type Shard struct {
	ID           int         // 0..S-1
	Items        []warm.Item // items for this shard, carrying their GLOBAL indices
	ManifestKey  string      // s3 key for this shard's manifest
	ResultPrefix string      // s3 prefix for this shard's results
	SummaryKey   string      // s3 key for this shard's run summary
	LogKey       string      // s3 key for this shard's bootstrap log
}

// ShardItems splits items into at most `shards` contiguous shards, preserving each
// item's global Index. Contiguous (not round-robin) so a shard owns a dense index
// range — cheaper to reason about and to re-drive (D4). Empty shards are dropped
// (shards > len(items) yields fewer shards, never zero-item ones). Panics only on a
// non-positive shard count, which is a programming error, not user input.
func ShardItems(items []warm.Item, shards int) []Shard {
	if shards < 1 {
		shards = 1
	}
	if shards > len(items) {
		shards = len(items)
	}
	out := make([]Shard, 0, shards)
	if len(items) == 0 {
		return out
	}
	// Balanced contiguous split: the first (n mod shards) shards get one extra item.
	n := len(items)
	base := n / shards
	extra := n % shards
	start := 0
	for s := 0; s < shards; s++ {
		size := base
		if s < extra {
			size++
		}
		if size == 0 {
			continue
		}
		out = append(out, Shard{ID: s, Items: items[start : start+size]})
		start += size
	}
	return out
}

// ShardLayout derives a shard's S3 keys under a run base, so every shard writes to
// a distinct manifest/result/summary/log namespace but shares the artifact prefix.
func ShardLayout(base string, artifactPfx string, shardID int) (manifestKey, resultPrefix, summaryKey, logKey string) {
	sd := fmt.Sprintf("%s/shard-%d", base, shardID)
	return sd + "/manifest.json", sd + "/results", sd + "/summary.json", sd + "/bootstrap.log"
}

// CollectShards folds the results of every shard into one globally-ordered set plus
// one global missing[] (spec §10/§15: partial failure across instances). It reuses
// the single-prefix Collect per shard, then merges via mergeShardResults — an
// instance that dropped items surfaces those global indices in one combined missing
// list rather than a crash.
func CollectShards(ctx context.Context, client *s3.Client, bucket string, shards []Shard) (results []warm.Result, missing []int, err error) {
	perShard := make([][]warm.Result, len(shards))
	for i, sh := range shards {
		// Per-shard collect keyed by GLOBAL index (warmd wrote results under the
		// item's global Index, so Collect returns them global-indexed already). We
		// pass expected=0 to skip Collect's own contiguous-missing scan and compute
		// missing ourselves from each shard's actual item set (which is sparse in
		// global-index space once split).
		res, _, cerr := Collect(ctx, client, bucket, sh.ResultPrefix, 0)
		if cerr != nil {
			return nil, nil, fmt.Errorf("shard %d collect: %w", sh.ID, cerr)
		}
		perShard[i] = res
	}
	results, missing = mergeShardResults(shards, perShard)
	return results, missing, nil
}

// mergeShardResults is the pure fold behind CollectShards (unit-testable without
// S3): given each shard's collected results, it returns the globally-ordered union
// plus the global missing[] computed from every shard's expected item set.
func mergeShardResults(shards []Shard, perShard [][]warm.Result) (results []warm.Result, missing []int) {
	got := map[int]warm.Result{}
	missSet := map[int]bool{}
	for i, sh := range shards {
		landed := make(map[int]bool, len(perShard[i]))
		for _, r := range perShard[i] {
			got[r.Index] = r
			landed[r.Index] = true
		}
		for _, it := range sh.Items {
			if !landed[it.Index] {
				missSet[it.Index] = true
			}
		}
	}
	results = make([]warm.Result, 0, len(got))
	for _, r := range got {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	for idx := range missSet {
		missing = append(missing, idx)
	}
	sort.Ints(missing)
	return results, missing
}
