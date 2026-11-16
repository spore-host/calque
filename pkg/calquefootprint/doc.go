// Package calquefootprint inventories, classifies and tears down what calque
// created in an AWS account, on the shared leave-no-trace schema
// (github.com/spore-host/libs/footprint; spore-host/spore-host#752).
//
// It is the engine behind `calque footprint` and `calque teardown`, and the
// module spawn's reaper imports so its account-idle sweep covers calque
// (spore-host/spawn#1654). For that it depends only on the AWS SDK and
// spore-host/libs, never on spawn, lagotto, truffle or cohort: a test
// (TestDependencies) fails the build if that changes.
//
// # What it finds
//
//   - IAM roles and instance profiles calque-real-run-<hash>,
//     calque-pool-worker and calque-fleet-worker, and the per-queue inline
//     grants (q-<hash>) on the two worker roles (calque#226);
//   - SQS queues calque-pool-* and calque-fleet-*, with their message counts
//     and live workers;
//   - the calque-nfs-sg security group in each VPC, with its network
//     interfaces;
//   - AMIs and EBS snapshots tagged calque:baked-image;
//   - instances tagged calque:managed=true that are not terminated;
//   - S3 objects tagged spore-host:managed-by=calque under runs/, fleet/ and
//     ami-bake/, in the buckets the caller names. calque writes into buckets
//     its user chooses and records none of them, so it cannot find them on
//     its own: a bucket not passed in Options.Buckets is not looked at.
//
// Classify then judges each resource (see its doc), and footprint.Deletable
// gives the verdict: only a resource tagged managed-by=calque, named like
// one of Patterns, and not user data is ever deletable. A resource that
// matched only by name (made before calque#227 tagged everything) is
// reported, never deleted.
//
// # Use from the reaper
//
// The reaper builds Clients for the account it sweeps, lists every enabled
// region, and classifies:
//
//	clients := calquefootprint.ClientsFromConfig(cfg)
//	regions, err := calquefootprint.EnabledRegions(ctx, ec2.NewFromConfig(cfg))
//	fp, err := calquefootprint.Inventory(ctx, clients, calquefootprint.Options{
//		Account: account, Regions: regions, AllRegions: true,
//	}, time.Now())
//	// fp.Report is a footprint.Report: log it (report mode), or judge
//	// calque idle when no resource is active:
//	idle := calquefootprint.Idle(fp.Report)
//
// Pass AllRegions only with every enabled region: an IAM role is judged
// unused only if no instance in any region uses it. The reaper has no
// buckets to pass, so S3 objects are left to `calque teardown --bucket`.
//
// To reap, once it holds the teardown lease (libs/lifecycle):
//
//	plan, err := footprint.NewPlan(calquefootprint.Tool, calquefootprint.Patterns(), fp.Report.Resources)
//	res, err := footprint.Execute(ctx, plan, footprint.ModeReap,
//		calquefootprint.NewDeleter(clients, calquefootprint.DeleterOptions{Regions: regions}), lease)
//
// The Deleter re-checks every resource before deleting it, and refuses
// (ErrActive) one that is in use by then.
package calquefootprint
