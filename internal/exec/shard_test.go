package exec

import (
	"reflect"
	"testing"

	warm "github.com/spore-host/calque/worker/warm-runner"
)

func items(n int) []warm.Item {
	out := make([]warm.Item, n)
	for i := range out {
		out[i] = warm.Item{Index: i, Payload: i}
	}
	return out
}

// TestShardItemsBalancedContiguous (D1): N items split into S contiguous shards
// with balanced sizes, global indices preserved, union == [0,N).
func TestShardItemsBalancedContiguous(t *testing.T) {
	shards := ShardItems(items(10), 3)
	if len(shards) != 3 {
		t.Fatalf("shards = %d, want 3", len(shards))
	}
	// Balanced: 10 / 3 -> sizes 4,3,3.
	wantSizes := []int{4, 3, 3}
	seen := map[int]bool{}
	for i, sh := range shards {
		if len(sh.Items) != wantSizes[i] {
			t.Errorf("shard %d size = %d, want %d", i, len(sh.Items), wantSizes[i])
		}
		for _, it := range sh.Items {
			if seen[it.Index] {
				t.Errorf("index %d appears in more than one shard", it.Index)
			}
			seen[it.Index] = true
		}
	}
	for i := 0; i < 10; i++ {
		if !seen[i] {
			t.Errorf("index %d missing from the union of shards", i)
		}
	}
	// Contiguity: shard 0 owns [0..3], shard 1 [4..6], shard 2 [7..9].
	if shards[0].Items[0].Index != 0 || shards[2].Items[len(shards[2].Items)-1].Index != 9 {
		t.Errorf("shards not contiguous: %v", shards)
	}
}

// TestShardItemsFewerItemsThanShards: never emits zero-item shards.
func TestShardItemsFewerItemsThanShards(t *testing.T) {
	shards := ShardItems(items(2), 5)
	if len(shards) != 2 {
		t.Fatalf("shards = %d, want 2 (one per item, no empties)", len(shards))
	}
	for _, sh := range shards {
		if len(sh.Items) == 0 {
			t.Error("empty shard emitted")
		}
	}
}

// TestMergeShardResultsUnionAndMissing (D3): the union is globally ordered and the
// global missing[] spans a dropped item in ANY shard.
func TestMergeShardResultsUnionAndMissing(t *testing.T) {
	all := items(10)
	shards := ShardItems(all, 2) // shard0: [0..4], shard1: [5..9]
	// shard0 returns all its items; shard1 DROPS index 7 (partial failure).
	res0 := make([]warm.Result, 0)
	for _, it := range shards[0].Items {
		res0 = append(res0, warm.Result{Index: it.Index, Result: it.Index})
	}
	var res1 []warm.Result
	for _, it := range shards[1].Items {
		if it.Index == 7 {
			continue // dropped
		}
		res1 = append(res1, warm.Result{Index: it.Index, Result: it.Index})
	}

	results, missing := mergeShardResults(shards, [][]warm.Result{res0, res1})
	if len(results) != 9 {
		t.Errorf("results = %d, want 9 (one dropped)", len(results))
	}
	// Globally ordered.
	for i := 1; i < len(results); i++ {
		if results[i-1].Index >= results[i].Index {
			t.Errorf("results not globally ordered at %d: %v", i, results)
		}
	}
	if !reflect.DeepEqual(missing, []int{7}) {
		t.Errorf("missing = %v, want [7]", missing)
	}
}

// TestShardLayoutDistinctNamespaces: each shard writes to a distinct manifest /
// result / summary / log key so instances never collide in S3.
func TestShardLayoutDistinctNamespaces(t *testing.T) {
	m0, r0, s0, l0 := ShardLayout("runs/x", "runs/x/artifacts", 0)
	m1, r1, _, _ := ShardLayout("runs/x", "runs/x/artifacts", 1)
	if m0 == m1 || r0 == r1 {
		t.Errorf("shard namespaces collide: %s/%s vs %s/%s", m0, r0, m1, r1)
	}
	for _, k := range []string{m0, r0, s0, l0} {
		if k == "" {
			t.Error("empty shard key")
		}
	}
}
