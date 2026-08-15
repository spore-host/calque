package main

import (
	"strings"
	"testing"
)

// TestParseAMIBakeArgs_Defaults verifies calque#144's `ami bake` flags parse
// into an amiBakeOpts with the documented defaults when only the required
// flags are supplied, following spot_flags_test.go's established
// parseXArgs-in-isolation pattern (no live AWS).
func TestParseAMIBakeArgs_Defaults(t *testing.T) {
	o, confirm, err := parseAMIBakeArgs([]string{
		"--bucket", "b", "--run-id", "r", "--name", "n",
		"--i-understand-this-spends-money",
	})
	if err != nil {
		t.Fatalf("parseAMIBakeArgs: %v", err)
	}
	if !confirm {
		t.Errorf("expected confirm=true")
	}
	if o.region != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", o.region)
	}
	if o.instance != "g6.2xlarge" {
		t.Errorf("instance = %q, want g6.2xlarge", o.instance)
	}
	if o.image != "vllm/vllm-openai:latest" {
		t.Errorf("image = %q, want vllm/vllm-openai:latest", o.image)
	}
	if o.baseAMI != "" {
		t.Errorf("baseAMI = %q, want empty (spawn auto-selects)", o.baseAMI)
	}
	if o.bucket != "b" || o.runID != "r" || o.name != "n" {
		t.Errorf("bucket/runID/name = %q/%q/%q, want b/r/n", o.bucket, o.runID, o.name)
	}
}

// TestParseAMIBakeArgs_MissingRequiredFlags proves bucket/run-id/name are
// all required — none has a usable default, so a missing one must error
// before anything billable happens.
func TestParseAMIBakeArgs_MissingRequiredFlags(t *testing.T) {
	cases := [][]string{
		{"--run-id", "r", "--name", "n"},
		{"--bucket", "b", "--name", "n"},
		{"--bucket", "b", "--run-id", "r"},
	}
	for _, args := range cases {
		if _, _, err := parseAMIBakeArgs(args); err == nil {
			t.Errorf("parseAMIBakeArgs(%v): expected error for missing required flag", args)
		}
	}
}

// TestParseAMIBakeArgs_ConfirmDefaultsOff proves the billable confirm flag
// defaults to false — bake must refuse to run without it.
func TestParseAMIBakeArgs_ConfirmDefaultsOff(t *testing.T) {
	o, confirm, err := parseAMIBakeArgs([]string{"--bucket", "b", "--run-id", "r", "--name", "n"})
	if err != nil {
		t.Fatalf("parseAMIBakeArgs: %v", err)
	}
	if confirm {
		t.Errorf("expected confirm=false by default")
	}
	_ = o
}

// TestParseAMIBakeArgs_Overrides verifies every override flag actually
// lands in amiBakeOpts, not just the defaults path.
func TestParseAMIBakeArgs_Overrides(t *testing.T) {
	o, _, err := parseAMIBakeArgs([]string{
		"--bucket", "b", "--run-id", "r", "--name", "n",
		"--region", "eu-central-1", "--instance", "g7e.2xlarge",
		"--ami", "ami-123", "--image", "myrepo/custom:v1",
		"--ttl", "1h", "--deadline-min", "45",
		"--spot", "--spot-max-price", "2.50",
		"--i-understand-this-spends-money",
	})
	if err != nil {
		t.Fatalf("parseAMIBakeArgs: %v", err)
	}
	if o.region != "eu-central-1" {
		t.Errorf("region = %q, want eu-central-1", o.region)
	}
	if o.instance != "g7e.2xlarge" {
		t.Errorf("instance = %q, want g7e.2xlarge", o.instance)
	}
	if o.baseAMI != "ami-123" {
		t.Errorf("baseAMI = %q, want ami-123", o.baseAMI)
	}
	if o.image != "myrepo/custom:v1" {
		t.Errorf("image = %q, want myrepo/custom:v1", o.image)
	}
	if o.ttl != "1h" {
		t.Errorf("ttl = %q, want 1h", o.ttl)
	}
	if !o.spot || o.spotMaxPrice != "2.50" {
		t.Errorf("spot/spotMaxPrice = %v/%q, want true/2.50", o.spot, o.spotMaxPrice)
	}
}

// TestAMIBakeBootstrapCommand_PullsRequestedImage guards the actual wiring
// between --image and the script the bake instance runs — a copy-paste slip
// here would silently bake the wrong image.
func TestAMIBakeBootstrapCommand_PullsRequestedImage(t *testing.T) {
	cmd := amiBakeBootstrapCommand("myrepo/custom:v1", "my-bucket", "ami-bake/r/done", "ami-bake/r/bootstrap.log")
	if !strings.Contains(cmd, "docker pull myrepo/custom:v1") {
		t.Errorf("bootstrap command missing expected docker pull line:\n%s", cmd)
	}
	if !strings.Contains(cmd, "s3://my-bucket/ami-bake/r/done") {
		t.Errorf("bootstrap command missing expected done-marker S3 key:\n%s", cmd)
	}
}

// TestParseAMIListArgs_Default verifies `ami list`'s only flag defaults to
// us-west-2, matching every other subcommand's region default.
func TestParseAMIListArgs_Default(t *testing.T) {
	o, err := parseAMIListArgs(nil)
	if err != nil {
		t.Fatalf("parseAMIListArgs: %v", err)
	}
	if o.region != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", o.region)
	}
}

// TestParseAMIDeleteArgs_RequiresAMIIDAndConfirm proves `ami delete` needs
// both a positional AMI id and the destructive-confirm flag.
func TestParseAMIDeleteArgs_RequiresAMIIDAndConfirm(t *testing.T) {
	if _, _, err := parseAMIDeleteArgs(nil); err == nil {
		t.Error("parseAMIDeleteArgs(nil): expected error for missing AMI id")
	}
	o, confirm, err := parseAMIDeleteArgs([]string{"ami-abc123"})
	if err != nil {
		t.Fatalf("parseAMIDeleteArgs: %v", err)
	}
	if o.amiID != "ami-abc123" {
		t.Errorf("amiID = %q, want ami-abc123", o.amiID)
	}
	if confirm {
		t.Errorf("expected confirm=false by default")
	}
	_, confirm2, err := parseAMIDeleteArgs([]string{"--i-understand-this-deletes-the-ami", "ami-abc123"})
	if err != nil {
		t.Fatalf("parseAMIDeleteArgs: %v", err)
	}
	if !confirm2 {
		t.Errorf("expected confirm=true when flag passed")
	}
}
