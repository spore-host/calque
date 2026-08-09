// Command gpuprobe is a ONE-OFF hardware-verification tool for calque#104: it
// acquires a real GPU instance, runs nvidia-smi/MIG/MPS diagnostics over SSM,
// prints the results, and terminates. Not part of calque's product surface —
// this exists only to answer "does this specific AWS-deployed card support
// MIG/MPS," a question no amount of datasheet research can settle (see
// docs/gpu-sharing-support-matrix.md, which this tool's output feeds).
//
// Usage:
//
//	gpuprobe --instance g7.2xlarge --region us-east-1 --ami ami-xxxx --i-understand-this-spends-money
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	spawnaws "github.com/spore-host/spawn/pkg/aws"

	calexec "github.com/spore-host/calque/internal/exec"
	"github.com/spore-host/calque/internal/leak"
	"github.com/spore-host/calque/internal/plan"
	"github.com/spore-host/calque/internal/target"
)

// probeScript runs entirely over SSM (no user-data, no docker) — the instance
// boots plain, SSM agent comes online, we push this script, capture output,
// then terminate. Exit code is always 0 so a MIG-absent card (a real, expected
// finding) doesn't read as a probe failure — the STDOUT is the finding.
const probeScript = `
echo "=== nvidia-smi -L ==="
nvidia-smi -L 2>&1 || echo "(nvidia-smi -L failed)"
echo
echo "=== nvidia-smi (driver/card summary) ==="
nvidia-smi 2>&1 | head -20 || echo "(nvidia-smi failed)"
echo
echo "=== MPS FIRST (before any MIG mode change, which needs a GPU reset and can leave the card in a transitional state that breaks a plain CUDA context) ==="
echo "--- nvidia-cuda-mps-control availability ---"
which nvidia-cuda-mps-control 2>&1 || echo "(nvidia-cuda-mps-control not on PATH)"
echo "--- attempt: start MPS control daemon ---"
sudo mkdir -p /tmp/nvidia-mps /tmp/nvidia-log
sudo sh -c 'export CUDA_VISIBLE_DEVICES=0 CUDA_MPS_PIPE_DIRECTORY=/tmp/nvidia-mps CUDA_MPS_LOG_DIRECTORY=/tmp/nvidia-log && nohup nvidia-cuda-mps-control -d > /tmp/mps-start.log 2>&1' || echo "(mps-control -d launch failed)"
sleep 3
echo "--- mps-start.log ---"
cat /tmp/mps-start.log 2>&1
echo "--- ps for mps control process ---"
ps aux | grep -i mps | grep -v grep || echo "(no mps process found)"
echo "--- quit the daemon ---"
sudo sh -c 'export CUDA_MPS_PIPE_DIRECTORY=/tmp/nvidia-mps && echo quit | nvidia-cuda-mps-control' 2>&1 || echo "(mps-control quit failed)"
echo
echo "=== MIG (after MPS test, since enabling MIG mode needs a GPU reset) ==="
echo "--- nvidia-smi mig -lgip (list GPU instance profiles) ---"
sudo nvidia-smi mig -lgip 2>&1 || echo "(mig -lgip failed / not supported)"
echo "--- nvidia-smi mig -lgi (list existing GPU instances) ---"
sudo nvidia-smi mig -lgi 2>&1 || echo "(mig -lgi failed / not supported)"
echo "--- attempt: enable MIG mode ---"
sudo nvidia-smi -mig 1 2>&1 || echo "(nvidia-smi -mig 1 failed / not supported on this card)"
echo
echo "=== driver version ==="
cat /proc/driver/nvidia/version 2>&1 || echo "(no /proc/driver/nvidia/version)"
echo "=== DONE ==="
`

func main() {
	instance := flag.String("instance", "", "instance type to probe, e.g. g7.2xlarge (required)")
	region := flag.String("region", "us-east-1", "AWS region")
	ami := flag.String("ami", "", "AMI with NVIDIA drivers + SSM agent pre-installed (required)")
	ttl := flag.String("ttl", "20m", "instance TTL hard cap")
	deadlineMin := flag.Int("deadline-min", 15, "give up acquiring after N minutes")
	spot := flag.Bool("spot", false, "acquire on the Spot market (different capacity pool than on-demand)")
	confirm := flag.Bool("i-understand-this-spends-money", false, "required: launches a billable GPU instance")
	flag.Parse()

	if *instance == "" || *ami == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuprobe --instance g7.2xlarge --ami ami-xxxx [--region us-east-1] --i-understand-this-spends-money")
		os.Exit(2)
	}
	if !*confirm {
		fmt.Fprintln(os.Stderr, "refusing to launch: pass --i-understand-this-spends-money")
		os.Exit(2)
	}

	if err := probe(*instance, *region, *ami, *ttl, time.Duration(*deadlineMin)*time.Minute, *spot); err != nil {
		fmt.Fprintln(os.Stderr, "gpuprobe error:", err)
		os.Exit(1)
	}
}

func probe(instanceType, region, ami, ttl string, deadline time.Duration, spot bool) (err error) {
	ctx := context.Background()
	rep := &leak.Report{}
	fmt.Printf("=== gpuprobe: %s in %s (ami=%s) ===\n", instanceType, region, ami)

	cfg, cerr := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if cerr != nil {
		return cerr
	}
	spawnClient, serr := spawnaws.NewClientWithRegion(ctx, region)
	if serr != nil {
		return fmt.Errorf("spawn client: %w", serr)
	}

	var places []plan.Placement
	if found, aerr := calexec.AZsForInstance(ctx, ec2.NewFromConfig(cfg), instanceType); aerr == nil {
		for _, f := range found {
			places = append(places, plan.Placement{AZ: f.AZ, Subnet: f.Subnet})
		}
	}

	launchCfg := plan.SpawnLauncher{
		TTL: ttl, OnComplete: "terminate",
		Username: "ubuntu", AMI: ami, Spot: spot,
		// No RunCmd: boot plain, no docker/GPU job — this probe runs over SSM
		// after the instance is up, not via user-data.
	}.Build()
	acq := &plan.Acquirer{
		LaunchConfig: launchCfg, Report: rep, Deadline: deadline, Placements: places,
		OnProgress: func(attempt int, code, detail string, waited time.Duration) {
			fmt.Printf("      ...swept %d attempt(s), no capacity (%s, %s)\n", attempt, code, waited.Round(time.Second))
		},
	}
	tgt := &target.Target{Card: target.DefaultCard, Instance: instanceType}
	fmt.Printf("[1/3] acquiring %s in %s (block-and-wait, AZ-sweep)...\n", instanceType, region)
	acquired, aerr := acq.Acquire(ctx, tgt, region)
	if aerr != nil {
		return fmt.Errorf("acquire: %w", aerr)
	}
	fmt.Printf("      acquired %s (%s) after %s\n", acquired.InstanceID, acquired.AvailabilityZone, acquired.TimeToAcquire().Round(time.Second))

	defer func() {
		fmt.Printf("[3/3] terminating %s ...\n", acquired.InstanceID)
		if tErr := spawnClient.Terminate(context.Background(), acquired.Region, acquired.InstanceID); tErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: terminate failed for %s: %v (TTL %s will reap)\n", acquired.InstanceID, tErr, ttl)
			if err == nil {
				err = fmt.Errorf("terminate: %w", tErr)
			}
		} else {
			fmt.Printf("      terminated %s\n", acquired.InstanceID)
		}
	}()

	// Give the SSM agent time to register after boot before sending the command.
	fmt.Printf("[2/3] waiting for SSM agent to come online (up to 5m)...\n")
	if werr := spawnClient.WaitForSSMOnline(ctx, acquired.Region, acquired.InstanceID, 5*time.Minute); werr != nil {
		return fmt.Errorf("wait for SSM online: %w", werr)
	}
	fmt.Printf("      SSM online; running probe script...\n")
	res, rerr := spawnClient.RunShellScript(ctx, acquired.Region, acquired.InstanceID, probeScript, 3*time.Minute)
	if rerr != nil {
		return fmt.Errorf("run probe script: %w", rerr)
	}
	fmt.Printf("--- probe status: %s (exit %d) ---\n", res.Status, res.ResponseCode)
	fmt.Println(res.Stdout)
	if res.Stderr != "" {
		fmt.Fprintln(os.Stderr, "--- stderr ---")
		fmt.Fprintln(os.Stderr, res.Stderr)
	}
	return nil
}
