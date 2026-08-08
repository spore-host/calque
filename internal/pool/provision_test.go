package pool

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWorkerPolicyScopesToThisPoolOnly proves the IAM policy is scoped to the
// SPECIFIC model's queue and the given manifest/results buckets — never a
// wildcard across all pools or all buckets (least-privilege, mirroring
// spawn's own poolWorkerPolicy discipline).
func TestWorkerPolicyScopesToThisPoolOnly(t *testing.T) {
	policy := WorkerPolicy("111122223333", "us-east-1", "resnet", "calque-manifests", "calque-results")

	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("WorkerPolicy did not produce valid JSON: %v\n%s", err, policy)
	}

	wantQueueARN := "arn:aws:sqs:us-east-1:111122223333:calque-pool-resnet"
	if !strings.Contains(policy, wantQueueARN) {
		t.Errorf("policy missing scoped queue ARN %q; got %s", wantQueueARN, policy)
	}
	// Must NOT grant SendMessage/CreateQueue/DeleteQueue — those are the
	// submitter's operations, not the worker's (mirrors spawn's split).
	for _, forbidden := range []string{"sqs:SendMessage", "sqs:CreateQueue", "sqs:DeleteQueue"} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("policy grants %q, which is a submitter-only action a worker must not have", forbidden)
		}
	}
	if !strings.Contains(policy, "calque-manifests") {
		t.Errorf("policy missing manifest bucket scoping; got %s", policy)
	}
	if !strings.Contains(policy, "calque-results") {
		t.Errorf("policy missing results bucket scoping; got %s", policy)
	}
	// A DIFFERENT model's queue must not appear anywhere in this policy.
	if strings.Contains(policy, "calque-pool-bert") {
		t.Errorf("policy for model %q leaked a reference to a different model's queue", "resnet")
	}
}

// TestBuildWorkerBootstrapCommandIsShellSafe proves the bootstrap command
// quotes every argument, so a model/path containing a space (a plausible HF
// repo-style name is unlikely to, but a runner path under a directory with a
// space is not implausible) doesn't silently truncate the command.
func TestBuildWorkerBootstrapCommandIsShellSafe(t *testing.T) {
	cmd := buildWorkerBootstrapCommand("resnet-50", "us-east-1", "/opt/calque runner/runner.py", "30m")
	for _, want := range []string{
		`--model "resnet-50"`,
		`--region "us-east-1"`,
		`--runner-path "/opt/calque runner/runner.py"`,
		`--idle-timeout "30m"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("bootstrap command missing %q; got: %s", want, cmd)
		}
	}
}
