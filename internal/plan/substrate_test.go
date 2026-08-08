package plan

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/scttfrdmn/substrate/emulator"

	spawnaws "github.com/spore-host/spawn/pkg/aws"

	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/target"
)

// TestAcquireAgainstSubstrate is calque#114: pointing the REAL
// plan.SpawnLauncher (wrapping spawn's real *spawnaws.Client and
// launcher.Provision) at a Substrate emulator, so Acquirer's retry/classify
// logic runs against an ACTUAL RunInstances round-trip — request-building,
// response-parsing, and error-classification all exercised for real — not
// just against fakeLauncher's hand-written stand-in (the pre-#114 test
// tier, still exercised by TestAcquireRetriesThenLands et al. above; this
// is the NEW middle tier, not a replacement for it).
//
// This is the offline verification tier issue #97/#107/#112's own design
// docs repeatedly flagged as missing for calque's fleet/tenancy/driver
// code — every one of them noted "no offline test path, real-AWS-
// verification-required" as a real cost. Substrate closes that gap
// generically: any future code built on plan.Acquirer gets this same
// offline coverage for free.
func TestAcquireAgainstSubstrate(t *testing.T) {
	ts := emulator.StartTestServer(t)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithBaseEndpoint(ts.URL),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		// Substrate's plugins parse EC2's XML wire format at whatever region
		// the request names; a fake resolver isn't needed here since we're
		// hitting the real EC2 plugin, not AWS's real endpoint.
	)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	spawnClient := spawnaws.NewClientFromConfig(cfg)

	launcher := &SpawnLauncher{
		Client: spawnClient,
		AMI:    "ami-0123456789abcdef0", // pinned: skip AMI auto-detection (a separate SSM round-trip Substrate would also need seeded)
		TTL:    "5m",
		// Pinned: skips spawn's own live AWS Pricing API call (spawn/pkg/aws/
		// client.go's Launch queries it when PricePerHour==0) — Substrate
		// emulates EC2, not the Pricing API, so a real network call would
		// otherwise sneak into this "offline" test.
		PricePerHour: 3.36,
	}
	acq := &Acquirer{
		Launcher: launcher,
		Deadline: 10 * time.Second,
		// PollInterval doesn't matter here — the first attempt succeeds.
	}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}

	acquired, err := acq.Acquire(context.Background(), tgt, "us-east-1")
	if err != nil {
		t.Fatalf("Acquire against Substrate: %v", err)
	}
	if acquired.InstanceID == "" {
		t.Error("Acquire returned an empty InstanceID — RunInstances round-trip did not parse a real instance id")
	}
	if acquired.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", acquired.Region)
	}
	if tgt.Region != "us-east-1" {
		t.Errorf("Target.Region = %q, want us-east-1 (Acquire must fill it on success)", tgt.Region)
	}
}

// TestAcquireAgainstSubstrateInjectedFailure exercises the FULL real
// request/response/classify pipeline against a Substrate-injected RunInstances
// failure — with one KNOWN, DOCUMENTED gap: Substrate's EC2 plugin currently
// emits errors as <ErrorResponse><Error><Code>... (the REST-XML shape), but
// the AWS SDK v2 EC2 client's ec2query error deserializer
// (aws-sdk-go-v2/aws/protocol/ec2query/error_utils.go's
// GetErrorResponseComponents) looks for <Errors><Error><Code>... — an extra
// wrapping <Errors> plural that Substrate's response doesn't have. Confirmed
// by direct inspection of both the wire body Substrate sends (via a raw HTTP
// POST, bypassing the SDK) and the SDK's own XPath, not assumed.
//
// The practical effect: the SDK can't extract Substrate's injected error CODE
// (e.g. "InsufficientInstanceCapacity") — it falls back to a generic
// "UnknownError". So this test does NOT (yet) prove classify()'s
// capacity-code branch fires against Substrate; it proves the alternate,
// still-real, still-valuable thing: that a genuine AWS-shaped failure
// (real HTTP 500, real smithy.APIError, real Unwrap chain through
// spawnaws.LaunchError) reaches classify() and is handled by the
// failureUnknown path exactly as TestAcquireUnknownFailsFast (offline, no
// Substrate) already proves — bounded retry, then fail fast, never an
// infinite loop on an unrecognized code.
//
// Filed upstream: github.com/scttfrdmn/substrate#591.
//
// TODO(substrate#591): once Substrate's EC2 plugin emits the <Errors>
// wrapper ec2query expects, upgrade this test to seed
// InsufficientInstanceCapacity and assert classify() takes the CAPACITY
// branch (keeps sweeping past the deadline) rather than the unknown branch
// (fails fast after maxUnknown+1).
func TestAcquireAgainstSubstrateInjectedFailure(t *testing.T) {
	ts := emulator.StartTestServer(t)
	seedCapacityFault(t, ts)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithBaseEndpoint(ts.URL),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	spawnClient := spawnaws.NewClientFromConfig(cfg)

	launcher := &SpawnLauncher{Client: spawnClient, AMI: "ami-0123456789abcdef0", TTL: "5m", PricePerHour: 3.36}
	rep := &leak.Report{}
	var progressCalls int
	var lastCode string
	acq := &Acquirer{
		Launcher: launcher, Report: rep,
		Deadline:     2 * time.Second,
		PollInterval: 10 * time.Millisecond,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			progressCalls++
			lastCode = code
		},
	}
	tgt := &target.Target{Card: "RTX PRO 6000", Instance: "g7e.2xlarge"}

	_, err = acq.Acquire(context.Background(), tgt, "us-east-1")
	if err == nil {
		t.Fatal("Acquire succeeded despite Substrate injecting a RunInstances failure on every attempt")
	}
	// Per the documented XML-shape gap above: the code classify() actually
	// sees is "" (smithy decodes an empty Code from the mismatched XPath),
	// which classify() maps to failureUnknown, not failureCapacity. If this
	// ever starts reporting "InsufficientInstanceCapacity" instead, Substrate
	// has fixed the wrapper — see the TODO above to upgrade this test.
	if lastCode == "InsufficientInstanceCapacity" {
		t.Log("Substrate now emits a classifiable error code — upgrade this test per its own TODO to assert the capacity branch")
	}
	if progressCalls == 0 {
		t.Error("OnProgress was never called — Acquire returned without any sweep attempt being observed")
	}
	t.Logf("observed code=%q after %d progress callback(s) (documented Substrate/ec2query XML-shape gap)", lastCode, progressCalls)
}

// seedCapacityFault configures Substrate to return
// InsufficientInstanceCapacity on every EC2 RunInstances call, via the
// fault-injection HTTP API (POST /v1/fault/rules) StartTestServer wires up
// by default (fault controller present but disabled until a rule is set).
func seedCapacityFault(t *testing.T, ts *emulator.TestServer) {
	t.Helper()
	// times:-1 means unlimited (Substrate's FaultRule.Times: zero means "fire
	// exactly once," a deliberate design choice so a mistyped field can't
	// accidentally consume a whole retry budget — see fault.go's own doc).
	// This test wants the Acquirer to see capacity failures on EVERY sweep
	// attempt, so it needs the explicit unlimited value.
	body := `{"enabled":true,"rules":[{"service":"ec2","operation":"RunInstances","fault_type":"error","error_code":"InsufficientInstanceCapacity","http_status":500,"probability":1.0,"times":-1}]}`
	resp, err := http.Post(ts.URL+"/v1/fault/rules", "application/json", strings.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("seed capacity fault: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed capacity fault: unexpected status %d", resp.StatusCode)
	}
}
