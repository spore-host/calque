package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/efs"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	spawnstorage "github.com/spore-host/spawn/pkg/storage"

	"github.com/spore-host/calque/internal/ir"
	"github.com/spore-host/calque/internal/leak"
)

// NetworkFileSystem plumbing (calque#91 Workstream B).
//
// Modal's NetworkFileSystem.from_name(...), used as a network_file_systems=
// value, mounts a LIVE-SHARED filesystem into the container — a
// fundamentally different shape from Volume's snapshot-and-sync S3 model
// (see volume.go) or CloudBucketMount's direct-bucket mountpoint-s3 model
// (see cloudbucketmount.go): every container sees the SAME live filesystem
// state simultaneously, no download/commit cycle at all. The AWS analog is
// EFS (Elastic File System) mounted via NFS.
//
// Design decision (approved plan, not revisited here): bring-your-own EFS,
// never auto-created. Modal's own create_if_missing=False default means a
// script that doesn't ask for auto-create is already describing this same
// "the filesystem must already exist" posture — requiring a pre-existing
// EFS filesystem, discovered by a calque:nfs-name=<name> tag convention, is
// a faithful translation, not a lesser one. create_if_missing=True in the
// source script is a LEAK ("auto-create not supported; pre-provision an EFS
// filesystem tagged calque:nfs-name=<name>"), never a hard failure — see
// DiscoverEFSFilesystem's own doc comment for the tag convention itself.
//
// IAM is explicitly OUT of scope for this pass — no
// elasticfilesystem:ClientMount/ClientWrite policy statements are added
// anywhere in this file or internal/plan/iam.go.

// NFSMountResolved is one resolved NetworkFileSystem mount: the in-container
// mount path plus the Modal NetworkFileSystem name from the script's own
// from_name(...) call — mirrors CloudBucketMountResolved's shape.
type NFSMountResolved struct {
	MountPath string
	Name      string // Modal NetworkFileSystem name, e.g. "shared-fs"
}

// ResolveNetworkFileSystems collects every modal.NetworkFileSystem.
// from_name(...) mounted by the app's classes/functions, deduped by mount
// path — mirrors ResolveCloudBucketMounts'/ResolveVolumes' exact
// collect/conflict/sort shape, applied to the sibling NetworkFileSystems
// map instead. A mount path claimed by two DIFFERENT NetworkFileSystems is
// a conflict we leak rather than guess through, same as the other two.
func ResolveNetworkFileSystems(app ir.App, rep *leak.Report) []NFSMountResolved {
	seen := map[string]ir.NetworkFileSystemMount{} // mountPath -> resolved mount
	collect := func(owner string, mounts map[string]ir.NetworkFileSystemMount, line int) {
		for mountPath, m := range mounts {
			if m.Name == "" {
				continue
			}
			if prev, ok := seen[mountPath]; ok && prev != m {
				rep.Addf(leak.PrimVolume, leak.KindUnhandledCase, app.Script, line,
					"%s: mount path %q maps to two different NetworkFileSystems (%+v vs %+v); mount overlap not modeled",
					owner, mountPath, prev, m)
				continue
			}
			seen[mountPath] = m
		}
	}
	for _, c := range app.Classes {
		collect(c.Name, c.NetworkFileSystems, c.Line)
	}
	for _, f := range app.Functions {
		collect(f.Name, f.NetworkFileSystems, f.Line)
	}

	// Deterministic order (stable plans / stable tests).
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]NFSMountResolved, 0, len(paths))
	for _, p := range paths {
		out = append(out, NFSMountResolved{MountPath: p, Name: seen[p].Name})
	}
	return out
}

// nfsNameTag is the tag KEY convention DiscoverEFSFilesystem matches
// against (calque#91 Workstream B) — a NEW convention (CloudBucketMount's S3
// mount has no analogous discovery-by-tag need, since the script's own
// bucket_name IS the identifier already). An operator provisioning an EFS
// filesystem for a script's network_file_systems={"...": nfs} mount must tag
// it "calque:nfs-name" = the SAME string passed to
// NetworkFileSystem.from_name(...) in the script.
const nfsNameTag = "calque:nfs-name"

// DiscoverEFSFilesystem resolves name (the script's own
// NetworkFileSystem.from_name(...) argument) to a real, pre-provisioned EFS
// filesystem ID by matching the calque:nfs-name tag (see nfsNameTag) against
// every filesystem in the account/region. EFS's DescribeFileSystems has no
// server-side tag filter (unlike e.g. EC2's DescribeImages) but DOES return
// each filesystem's own Tags inline in FileSystemDescription — no separate
// ListTagsForResource round-trip is needed.
//
// Returns a hard error (not a leak) when zero filesystems carry the tag (a
// genuinely missing required resource — the operator must pre-provision it,
// per this package's bring-your-own design) or when more than one does
// (ambiguous — the operator must dedupe the tag before this can proceed).
func DiscoverEFSFilesystem(ctx context.Context, efsClient *efs.Client, name string) (string, error) {
	var matches []string
	paginator := efs.NewDescribeFileSystemsPaginator(efsClient, &efs.DescribeFileSystemsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("discover EFS filesystem for %q: describe file systems: %w", name, err)
		}
		for _, fs := range page.FileSystems {
			for _, tag := range fs.Tags {
				if tag.Key != nil && *tag.Key == nfsNameTag && tag.Value != nil && *tag.Value == name {
					if fs.FileSystemId != nil {
						matches = append(matches, *fs.FileSystemId)
					}
					break
				}
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no EFS filesystem tagged %s=%q found — network_file_systems={...: NetworkFileSystem.from_name(%q)} is bring-your-own (calque never auto-creates an EFS filesystem); pre-provision one and tag it %s=%q", nfsNameTag, name, name, nfsNameTag, name)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d EFS filesystems tagged %s=%q found (ambiguous) — exactly one filesystem must carry this tag", len(matches), nfsNameTag, name)
	}
}

// ResolveMountTargetsForAZs checks which of azs already have a live EFS
// mount target for filesystemID, returning the DNS name to mount at for
// each covered AZ plus the list of AZs with NO mount target at all.
// GetEFSDNSName's own result is filesystem-wide (not per-mount-target — EFS
// exposes ONE DNS name resolving differently depending on which AZ the
// resolving instance is in, per its own doc comment), so dnsPerAZ's values
// are identical strings for every covered AZ; this function's real job is
// determining COVERAGE, not varying the DNS string itself.
func ResolveMountTargetsForAZs(ctx context.Context, efsClient *efs.Client, filesystemID, region string, azs []string) (dnsPerAZ map[string]string, missingAZs []string, err error) {
	out, err := efsClient.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{FileSystemId: &filesystemID})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve mount targets for %s: describe mount targets: %w", filesystemID, err)
	}
	covered := map[string]bool{}
	for _, mt := range out.MountTargets {
		if mt.AvailabilityZoneName != nil {
			covered[*mt.AvailabilityZoneName] = true
		}
	}
	dns := spawnaws.GetEFSDNSName(filesystemID, region)
	dnsPerAZ = map[string]string{}
	for _, az := range azs {
		if covered[az] {
			dnsPerAZ[az] = dns
		} else {
			missingAZs = append(missingAZs, az)
		}
	}
	return dnsPerAZ, missingAZs, nil
}

// NFSMountCommands returns the shell lines to mount each resolved
// NetworkFileSystem via NFS. dnsByName maps a resolved mount's Name (the
// Modal NetworkFileSystem name, e.g. "shared-fs") to the EFS DNS name the
// caller resolved for it (DiscoverEFSFilesystem + spawnaws.GetEFSDNSName) —
// keyed by NAME rather than a single flat DNS string because two DIFFERENT
// NetworkFileSystems (distinct names, distinct mount paths) resolve to
// DIFFERENT EFS filesystems, each with its own DNS name; GetEFSDNSName's own
// per-filesystem result IS constant across every AZ (per its own doc
// comment), so no further per-AZ variation is needed once dnsByName is
// built. A mount whose Name has no entry in dnsByName is skipped (the
// caller's own resolution step already hard-errors before reaching here in
// that case — see cmd/calque/realrun.go's networkFileSystemSpecsForApp).
//
// Rendered lines: an idempotent nfs-common install check (matching this
// repo's existing `command -v X >/dev/null || (sudo apt-get update && sudo
// apt-get install -y X)` idiom, internal/exec/bootstrap.go), then `mkdir -p`
// the mount path, then `mount -t nfs4 -o <options> <dns>:/ <mountpath>`.
// Mount options come from spawn/pkg/storage's "general" EFS profile
// (spawnstorage.GetEFSProfile), falling back to an equivalent hand-rolled
// default if that call ever errors (it doesn't for the "general" profile
// today, but this avoids a mount line silently going missing if it ever
// did).
func NFSMountCommands(mounts []NFSMountResolved, dnsByName map[string]string) []string {
	opts, err := spawnstorage.GetEFSProfile(spawnstorage.EFSProfileGeneral)
	mountOpts := "nfsvers=4.1,rsize=1048576,wsize=1048576,hard,timeo=600,retrans=2,noresvport,_netdev"
	if err == nil {
		mountOpts = opts.ToMountString() + ",_netdev"
	}
	var lines []string
	for _, m := range mounts {
		dns, ok := dnsByName[m.Name]
		if !ok {
			continue
		}
		lines = append(lines,
			`command -v mount.nfs4 >/dev/null || (sudo apt-get update && sudo apt-get install -y nfs-common)`,
			fmt.Sprintf("mkdir -p %s", m.MountPath),
			fmt.Sprintf("mount -t nfs4 -o %s %s:/ %s", mountOpts, dns, m.MountPath),
		)
	}
	return lines
}

// nfsSecurityGroupName is the (idempotency) lookup key EnsureNFSSecurityGroup
// uses — a fixed name, one per VPC, reused across runs rather than created
// fresh every time (mirrors RealRunInstanceProfile's per-bucket IAM role
// reuse-not-recreate posture in iam.go).
const nfsSecurityGroupName = "calque-nfs-sg"

// EnsureNFSSecurityGroup resolves (creating if needed) a security group in
// vpcID allowing NFS (TCP/2049) ingress from ITSELF — the standard
// self-referential pattern letting any instance placed in this SG reach any
// EFS mount target also reachable from this SG (an EFS mount target's own
// SG must in turn allow ingress from THIS group; that association is the
// operator's responsibility when provisioning the mount target — outside
// this function's scope, which only creates/finds the launched-instance
// side of the pairing).
//
// Idempotent: if the group already exists (by name, mirroring
// nfsSecurityGroupName), CreateSecurityGroup fails with
// InvalidGroup.Duplicate and this falls back to DescribeSecurityGroups
// instead of erroring.
func EnsureNFSSecurityGroup(ctx context.Context, ec2Client *ec2.Client, vpcID string) (string, error) {
	if sgID, err := findNFSSecurityGroup(ctx, ec2Client, vpcID); err == nil && sgID != "" {
		return sgID, nil
	}

	desc := "calque NFS/EFS ingress (calque#91 Workstream B)"
	created, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   strPtr(nfsSecurityGroupName),
		Description: strPtr(desc),
		VpcId:       strPtr(vpcID),
	})
	if err != nil {
		// Idempotency: someone else (a concurrent run, or a prior one whose
		// DescribeSecurityGroups lookup above raced) already created it.
		if isDuplicateGroupErr(err) {
			return findNFSSecurityGroup(ctx, ec2Client, vpcID)
		}
		return "", fmt.Errorf("create NFS security group: %w", err)
	}
	sgID := *created.GroupId

	// Self-referential ingress: the SG's own ID isn't known until after
	// creation, so authorizing ingress FROM it is necessarily a second call.
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: strPtr(sgID),
		IpPermissions: []ec2types.IpPermission{
			{
				IpProtocol:       strPtr("tcp"),
				FromPort:         int32Ptr(2049),
				ToPort:           int32Ptr(2049),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: strPtr(sgID)}},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("authorize NFS ingress on %s: %w", sgID, err)
	}
	return sgID, nil
}

// DefaultVPCID resolves the account/region's default VPC ID — the same VPC
// AZsForInstance's own default-subnet sweep (internal/exec/azs.go) already
// implicitly targets, needed here because EnsureNFSSecurityGroup requires a
// VPC ID explicitly (CreateSecurityGroup has no "use the default VPC"
// implicit behavior the way RunInstances does for SubnetID).
func DefaultVPCID(ctx context.Context, ec2Client *ec2.Client) (string, error) {
	out, err := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: strPtr("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", fmt.Errorf("describe default vpc: %w", err)
	}
	if len(out.Vpcs) == 0 || out.Vpcs[0].VpcId == nil {
		return "", fmt.Errorf("no default VPC found in this account/region")
	}
	return *out.Vpcs[0].VpcId, nil
}

func findNFSSecurityGroup(ctx context.Context, ec2Client *ec2.Client, vpcID string) (string, error) {
	out, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2types.Filter{
			{Name: strPtr("group-name"), Values: []string{nfsSecurityGroupName}},
			{Name: strPtr("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe NFS security group: %w", err)
	}
	if len(out.SecurityGroups) == 0 {
		return "", fmt.Errorf("no existing %s security group in vpc %s", nfsSecurityGroupName, vpcID)
	}
	return *out.SecurityGroups[0].GroupId, nil
}

// isDuplicateGroupErr reports whether err is EC2's InvalidGroup.Duplicate —
// the error CreateSecurityGroup returns when a group with the requested
// name already exists in the VPC (matched by substring since smithy-go's
// APIError interface exposes ErrorCode() as a plain string, no distinct
// generated type per code the way some AWS SDKs model client errors).
func isDuplicateGroupErr(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidGroup.Duplicate"
	}
	return false
}

func strPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32 { return &i }
