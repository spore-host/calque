package plan

import (
	"strings"
	"testing"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

func TestResolveCloudBucketMountsDeduped(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "Scorer", CloudBucketMounts: map[string]ir.CloudBucketMount{
				"/data": {BucketName: "my-bucket", KeyPrefix: "foo/", ReadOnly: true},
			}},
		},
		Functions: []ir.Function{
			{Name: "download", CloudBucketMounts: map[string]ir.CloudBucketMount{
				"/data": {BucketName: "my-bucket", KeyPrefix: "foo/", ReadOnly: true},
			}},
		},
	}
	mounts := ResolveCloudBucketMounts(app, rep)
	if len(mounts) != 1 {
		t.Fatalf("mounts = %d, want 1 (same mount at same path, deduped)", len(mounts))
	}
	m := mounts[0]
	if m.MountPath != "/data" || m.BucketName != "my-bucket" || m.KeyPrefix != "foo/" || !m.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	if rep.Len() != 0 {
		t.Errorf("identical mounts should not leak: %+v", rep.Leaks)
	}
}

// TestCloudBucketMountConflictLeaks: two DIFFERENT CloudBucketMounts (here,
// different bucket names) at one mount path is a conflict surfaced, not
// guessed through — mirrors TestVolumeConflictLeaks.
func TestCloudBucketMountConflictLeaks(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{
		Script: "s.py",
		Classes: []ir.Class{
			{Name: "A", CloudBucketMounts: map[string]ir.CloudBucketMount{"/mnt": {BucketName: "bucket-one"}}, Line: 5},
			{Name: "B", CloudBucketMounts: map[string]ir.CloudBucketMount{"/mnt": {BucketName: "bucket-two"}}, Line: 9},
		},
	}
	ResolveCloudBucketMounts(app, rep)
	if rep.Len() == 0 {
		t.Error("two different CloudBucketMounts at one mount path should leak a conflict")
	}
}

func TestNoCloudBucketMountsNoMounts(t *testing.T) {
	rep := &leak.Report{}
	app := ir.App{Script: "s.py", Classes: []ir.Class{{Name: "C"}}}
	if got := ResolveCloudBucketMounts(app, rep); len(got) != 0 {
		t.Errorf("no CloudBucketMounts -> no mounts, got %+v", got)
	}
}

// TestMountCommandsShape proves MountCommands emits an idempotent
// mount-s3 install check, a mkdir, and a mount-s3 invocation carrying the
// real bucket name/mount path — mirrors TestSyncUsesSyncNotCp's shape-
// assertion style for the Volume sibling.
func TestMountCommandsShape(t *testing.T) {
	mounts := []CloudBucketMountResolved{{MountPath: "/data", BucketName: "my-bucket"}}
	lines := MountCommands(mounts)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "command -v mount-s3") {
		t.Errorf("must idempotently check for mount-s3, got:\n%s", joined)
	}
	if !strings.Contains(joined, "mkdir -p /data") {
		t.Errorf("must create the mount path, got:\n%s", joined)
	}
	if !strings.Contains(joined, "mount-s3 my-bucket /data") {
		t.Errorf("must mount the real bucket at the mount path, got:\n%s", joined)
	}
	if strings.Contains(joined, "--read-only") || strings.Contains(joined, "--prefix") {
		t.Errorf("no read_only/key_prefix set — must not emit those flags, got:\n%s", joined)
	}
}

// TestMountCommandsReadOnlyAndPrefix proves the --read-only/--prefix flags
// are rendered when the resolved mount asked for them.
func TestMountCommandsReadOnlyAndPrefix(t *testing.T) {
	mounts := []CloudBucketMountResolved{{MountPath: "/data", BucketName: "my-bucket", KeyPrefix: "foo/", ReadOnly: true}}
	lines := MountCommands(mounts)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "mount-s3 my-bucket /data --prefix foo/ --read-only") {
		t.Errorf("expected --prefix and --read-only on the mount-s3 line, got:\n%s", joined)
	}
}

// TestMountCommandsOrder proves mkdir happens before the mount-s3 call, and
// the install check happens before both.
func TestMountCommandsOrder(t *testing.T) {
	mounts := []CloudBucketMountResolved{{MountPath: "/data", BucketName: "my-bucket"}}
	lines := MountCommands(mounts)
	joined := strings.Join(lines, "\n")
	installIdx := strings.Index(joined, "command -v mount-s3")
	mkdirIdx := strings.Index(joined, "mkdir -p /data")
	mountIdx := strings.Index(joined, "mount-s3 my-bucket /data")
	if installIdx == -1 || mkdirIdx == -1 || mountIdx == -1 {
		t.Fatalf("missing expected lines in:\n%s", joined)
	}
	if installIdx >= mkdirIdx || mkdirIdx >= mountIdx {
		t.Errorf("expected order install < mkdir < mount, got indices %d, %d, %d in:\n%s", installIdx, mkdirIdx, mountIdx, joined)
	}
}
