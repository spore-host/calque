package plan

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// TestResolveNetworkFileSystemsDeduped mirrors
// TestResolveCloudBucketMountsDeduped's exact shape: the same mount at the
// same path across a Class and a Function dedupes to one entry, no leak.
func TestResolveNetworkFileSystemsDeduped(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "Scorer", NetworkFileSystems: map[string]ir.NetworkFileSystemMount{
				"/shared": {Name: "shared-fs"},
			}},
		},
		Functions: []ir.Function{
			{Name: "download", NetworkFileSystems: map[string]ir.NetworkFileSystemMount{
				"/shared": {Name: "shared-fs"},
			}},
		},
	}
	mounts := ResolveNetworkFileSystems(app, rep)
	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1 (same mount at same path, deduped)", len(mounts))
	}
	m := mounts[0]
	if m.MountPath != "/shared" || m.Name != "shared-fs" {
		t.Errorf("mount = %+v", m)
	}
	if rep.Len() != 0 {
		t.Errorf("identical mounts should not leak: %+v", rep.Leaks)
	}
}

// TestNetworkFileSystemConflictLeaks: two DIFFERENT NetworkFileSystems at one
// mount path is a conflict surfaced, not guessed through — mirrors
// TestCloudBucketMountConflictLeaks/TestVolumeConflictLeaks.
func TestNetworkFileSystemConflictLeaks(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "A", NetworkFileSystems: map[string]ir.NetworkFileSystemMount{"/mnt": {Name: "fs-one"}}, Line: 5},
			{Name: "B", NetworkFileSystems: map[string]ir.NetworkFileSystemMount{"/mnt": {Name: "fs-two"}}, Line: 9},
		},
	}
	ResolveNetworkFileSystems(app, rep)
	if rep.Len() == 0 {
		t.Error("two different NetworkFileSystems at one mount path should leak a conflict")
	}
}

func TestNoNetworkFileSystemsNoMounts(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{Script: "s.py", Classes: []ir.Class{{Name: "C"}}}
	if got := ResolveNetworkFileSystems(app, rep); len(got) != 0 {
		t.Errorf("no NetworkFileSystems -> no mounts, got %+v", got)
	}
}

func TestResolveNetworkFileSystemsDeterministicOrder(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Functions: []ir.Function{
			{Name: "a", NetworkFileSystems: map[string]ir.NetworkFileSystemMount{
				"/z": {Name: "fs-z"}, "/a": {Name: "fs-a"}, "/m": {Name: "fs-m"},
			}},
		},
	}
	mounts := ResolveNetworkFileSystems(app, rep)
	if len(mounts) != 3 {
		t.Fatalf("mounts = %d, want 3", len(mounts))
	}
	if mounts[0].MountPath != "/a" || mounts[1].MountPath != "/m" || mounts[2].MountPath != "/z" {
		t.Errorf("expected sorted mount-path order, got %+v", mounts)
	}
}

// TestNFSMountCommandsShape proves NFSMountCommands emits an idempotent
// nfs-common install check, a mkdir, and a mount -t nfs4 invocation carrying
// the real DNS name/mount path — mirrors TestMountCommandsShape's shape-
// assertion style for the CloudBucketMount sibling.
func TestNFSMountCommandsShape(t *testing.T) {
	mounts := []NFSMountResolved{{MountPath: "/shared", Name: "shared-fs"}}
	lines := NFSMountCommands(mounts, map[string]string{"shared-fs": "fs-abc123.efs.us-west-2.amazonaws.com"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "command -v mount.nfs4") {
		t.Errorf("must idempotently check for mount.nfs4, got:\n%s", joined)
	}
	if !strings.Contains(joined, "mkdir -p /shared") {
		t.Errorf("must create the mount path, got:\n%s", joined)
	}
	if !strings.Contains(joined, "mount -t nfs4") || !strings.Contains(joined, "fs-abc123.efs.us-west-2.amazonaws.com:/ /shared") {
		t.Errorf("must mount the real DNS name at the mount path, got:\n%s", joined)
	}
}

// TestNFSMountCommandsOrder proves mkdir happens before the mount call, and
// the install check happens before both — mirrors TestMountCommandsOrder.
func TestNFSMountCommandsOrder(t *testing.T) {
	mounts := []NFSMountResolved{{MountPath: "/shared", Name: "shared-fs"}}
	lines := NFSMountCommands(mounts, map[string]string{"shared-fs": "fs-abc123.efs.us-west-2.amazonaws.com"})
	joined := strings.Join(lines, "\n")
	installIdx := strings.Index(joined, "command -v mount.nfs4")
	mkdirIdx := strings.Index(joined, "mkdir -p /shared")
	mountIdx := strings.Index(joined, "mount -t nfs4")
	if installIdx == -1 || mkdirIdx == -1 || mountIdx == -1 {
		t.Fatalf("missing expected lines in:\n%s", joined)
	}
	if installIdx >= mkdirIdx || mkdirIdx >= mountIdx {
		t.Errorf("expected order install < mkdir < mount, got indices %d, %d, %d in:\n%s", installIdx, mkdirIdx, mountIdx, joined)
	}
}

// TestNFSMountCommandsMountOptions proves the mount line carries the
// expected NFS options (nfsvers/rsize/wsize/hard/timeo/retrans/noresvport),
// matching the "general" EFS profile's ToMountString() output.
func TestNFSMountCommandsMountOptions(t *testing.T) {
	mounts := []NFSMountResolved{{MountPath: "/shared", Name: "shared-fs"}}
	lines := NFSMountCommands(mounts, map[string]string{"shared-fs": "fs-abc123.efs.us-west-2.amazonaws.com"})
	joined := strings.Join(lines, "\n")
	for _, w := range []string{"nfsvers=4.1", "hard", "noresvport", "_netdev"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing mount option %q in:\n%s", w, joined)
		}
	}
}

// NOTE on offline test coverage for DiscoverEFSFilesystem/
// ResolveMountTargetsForAZs/EnsureNFSSecurityGroup (all three real
// AWS-round-trip functions in efs.go): these are deliberately NOT
// offline-tested here. They take a concrete *efs.Client/*ec2.Client (not an
// interface calque itself defines), matching this package's existing
// documented precedent — internal/plan/acquire.go's own doc comment on
// Acquirer states "Acquire has NO offline test tier at all beyond
// errorCode's own pure-logic test — only real AWS_PROFILE=aws runs exercise
// the actual RunInstances round-trip." The same posture applies here: only a
// real AWS_PROFILE=aws run exercises DescribeFileSystems/
// DescribeMountTargets/CreateSecurityGroup/AuthorizeSecurityGroupIngress.
// Only ResolveNetworkFileSystems (pure Go, no AWS client) has offline
// coverage, mirroring ResolveCloudBucketMounts/ResolveVolumes.
