package exec

import (
	"reflect"
	"strings"
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
	m0, r0, s0, l0 := ShardLayout("runs/x", "runs/x/artifacts", "0")
	m1, r1, _, _ := ShardLayout("runs/x", "runs/x/artifacts", "1")
	if m0 == m1 || r0 == r1 {
		t.Errorf("shard namespaces collide: %s/%s vs %s/%s", m0, r0, m1, r1)
	}
	for _, k := range []string{m0, r0, s0, l0} {
		if k == "" {
			t.Error("empty shard key")
		}
	}
}

// TestShardLayoutAcceptsCallableNameKeys (calque#110): ShardLayout's shard
// key is now a string, so a .spawn() driver can pass a callable name
// directly (not just a formatted int) and get a distinct namespace per
// callable — the whole point of the string-keyed sibling this issue adds.
func TestShardLayoutAcceptsCallableNameKeys(t *testing.T) {
	mA, rA, _, _ := ShardLayout("runs/x", "runs/x/artifacts", "worker_a")
	mB, rB, _, _ := ShardLayout("runs/x", "runs/x/artifacts", "worker_b")
	if mA == mB || rA == rB {
		t.Errorf("callable-name namespaces collide: %s/%s vs %s/%s", mA, rA, mB, rB)
	}
	if mA != "runs/x/shard-worker_a/manifest.json" {
		t.Errorf("manifest key = %q, want runs/x/shard-worker_a/manifest.json", mA)
	}
}

// TestShardLayoutSanitizesUnsafeKeyChars proves an arbitrary callable name
// containing S3-key-unsafe characters (a slash, which could otherwise escape
// the shard- namespace prefix) is sanitized rather than passed through raw.
func TestShardLayoutSanitizesUnsafeKeyChars(t *testing.T) {
	m, _, _, _ := ShardLayout("runs/x", "runs/x/artifacts", "worker/../escape")
	if strings.Contains(m, "shard-worker/../escape") {
		t.Errorf("manifest key was not sanitized, allows path traversal: %q", m)
	}
	if !strings.HasPrefix(m, "runs/x/shard-worker") {
		t.Errorf("manifest key = %q, want prefix runs/x/shard-worker", m)
	}
}

func namedShard(key string, indices ...int) NamedShard {
	its := make([]warm.Item, len(indices))
	for i, idx := range indices {
		its[i] = warm.Item{Index: idx, Payload: idx}
	}
	return NamedShard{Key: key, Items: its}
}

// TestMergeNamedShardResultsKeyedByCallable (calque#110): the string-keyed
// sibling of TestMergeShardResultsUnionAndMissing — each callable's results
// land under its OWN key, and a callable with a dropped item is reported
// missing by its KEY (a call identity), not by a global item index (there
// is no global index across independent spawned calls).
func TestMergeNamedShardResultsKeyedByCallable(t *testing.T) {
	shards := []NamedShard{
		namedShard("worker_a", 0),
		namedShard("worker_b", 0, 1), // worker_b's index 1 will be DROPPED
	}
	resA := []warm.Result{{Index: 0, Result: "a-done"}}
	var resB []warm.Result
	resB = append(resB, warm.Result{Index: 0, Result: "b-done"}) // index 1 missing

	results, missing := mergeNamedShardResults(shards, [][]warm.Result{resA, resB})

	if len(results["worker_a"]) != 1 || results["worker_a"][0].Result != "a-done" {
		t.Errorf("results[worker_a] = %v, want one result 'a-done'", results["worker_a"])
	}
	if len(results["worker_b"]) != 1 || results["worker_b"][0].Result != "b-done" {
		t.Errorf("results[worker_b] = %v, want one result 'b-done'", results["worker_b"])
	}
	if !reflect.DeepEqual(missing, []string{"worker_b"}) {
		t.Errorf("missing = %v, want [worker_b] (its index 1 never landed)", missing)
	}
}

// TestMergeNamedShardResultsAllComplete proves a fully-landed set of named
// shards reports NO missing keys.
func TestMergeNamedShardResultsAllComplete(t *testing.T) {
	shards := []NamedShard{namedShard("worker_a", 0), namedShard("worker_b", 0)}
	resA := []warm.Result{{Index: 0, Result: "a"}}
	resB := []warm.Result{{Index: 0, Result: "b"}}

	_, missing := mergeNamedShardResults(shards, [][]warm.Result{resA, resB})
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none (every shard fully landed)", missing)
	}
}

// TestMergeNamedShardResultsSortsWithinEachKey proves a multi-item callable's
// (e.g. a .spawn()'d call with several args) own results come back ordered
// by its own item index, even though there is no cross-key global order.
func TestMergeNamedShardResultsSortsWithinEachKey(t *testing.T) {
	shards := []NamedShard{namedShard("worker_a", 0, 1, 2)}
	// Deliberately out of order, as a real S3 listing might return them.
	res := []warm.Result{
		{Index: 2, Result: "c"}, {Index: 0, Result: "a"}, {Index: 1, Result: "b"},
	}
	results, _ := mergeNamedShardResults(shards, [][]warm.Result{res})
	got := results["worker_a"]
	if len(got) != 3 || got[0].Result != "a" || got[1].Result != "b" || got[2].Result != "c" {
		t.Errorf("worker_a results = %v, want ordered [a, b, c]", got)
	}
}
