package calquefootprint

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
)

// Footprint is calque's footprint in one account: the shared
// footprint.Report, plus calque's per-resource Details (message and worker
// counts, ENIs, the instances using a role), keyed by Resource.ARN.
//
// Its JSON is the Report's, with one more top-level field, "details". The
// schema allows added fields, so footprint.Decode reads it as a Report.
type Footprint struct {
	Report  *footprint.Report
	Details map[string]Detail
	// Incomplete are the listings that failed (Snapshot.Incomplete), sorted,
	// and AllRegions is Snapshot.AllRegions. Neither is in the JSON; Idle
	// reads them.
	Incomplete []string `json:"-"`
	AllRegions bool     `json:"-"`
}

// Detail is what calque saw about one resource, beyond the Report schema.
type Detail struct {
	// Why says what decided the resource's class.
	Why string `json:"why"`
	// Queue is set for an SQS queue.
	Queue *QueueDetail `json:"queue,omitempty"`
	// ENIs are a security group's network interfaces.
	ENIs []string `json:"enis,omitempty"`
	// UsedBy are the live instances using a role or instance profile.
	UsedBy []string `json:"used_by,omitempty"`
	// ManagedPolicies are the managed policies attached to a role. Teardown
	// detaches them and never deletes them.
	ManagedPolicies []string `json:"managed_policies,omitempty"`
	// Grants is the queue an inline grant (q-*) grants.
	Grants string `json:"grants,omitempty"`
}

// QueueDetail is a queue's backlog and workers.
type QueueDetail struct {
	Visible  int      `json:"visible"`
	InFlight int      `json:"in_flight"`
	Delayed  int      `json:"delayed"`
	Workers  []string `json:"workers"`
}

// MarshalJSON writes the Report with a "details" object added.
func (f Footprint) MarshalJSON() ([]byte, error) {
	rep := f.Report
	if rep == nil {
		rep = &footprint.Report{Schema: footprint.SchemaVersion, Tool: Tool}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	details := f.Details
	if details == nil {
		details = map[string]Detail{}
	}
	if m["details"], err = json.Marshal(details); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (f *Footprint) UnmarshalJSON(b []byte) error {
	var rep footprint.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return err
	}
	var d struct {
		Details map[string]Detail `json:"details"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	f.Report, f.Details = &rep, d.Details
	return nil
}

// Classify judges a Snapshot as of now: each resource's ownership, class,
// dependencies and Deletable verdict. grace <= 0 means Grace.
//
// Ownership is tagged when spore-host:managed-by=calque, foreign when it
// names another tool, and legacy-name otherwise (the name, or a calque:* tag,
// matched a resource calque made before calque#227). Classes:
//
//   - active: an instance; a queue with workers, messages, or created within
//     the grace; a security group with ENIs; a role or profile a live
//     instance uses, or whose queue grant is active; an S3 object written
//     within the grace; and anything Classify cannot see enough of to rule
//     out activity (a failed listing, or a global resource when not every
//     region was listed);
//   - user-data: baked AMIs and their snapshots, which the user built and
//     removes with `calque ami delete`, and a run's results and summary;
//   - litter: the rest (idle queues, unused roles and profiles, grants whose
//     queue is gone or idle, security groups without ENIs, S3 staging older
//     than the grace).
//
// DependsOn: an instance profile, and each queue grant, on its role; a queue
// on its grant and its role; an instance on its profile; an AMI on its
// snapshots. A resource is deleted before what it depends on, so a profile
// and a grant go before their role, and a kept queue keeps its grant and
// role.
func Classify(s *Snapshot, now time.Time, grace time.Duration) *Footprint {
	if grace <= 0 {
		grace = Grace
	}
	c := &classifier{s: s, now: now.UTC(), grace: grace, details: map[string]Detail{}}
	c.index()
	rep := footprint.NewReport(Tool, s.Account, now)
	rep.Resources = c.resources()
	rep.Warnings = append(rep.Warnings, s.Warnings...)
	for _, k := range sortedKeys(s.Incomplete) {
		rep.Warnings = append(rep.Warnings, "listing failed: "+k+"; what it would have shown is treated as active")
	}
	if !s.AllRegions {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"only %s listed: IAM roles and profiles are classed active, since an instance in another region could use them (use --all-regions)",
			strings.Join(s.Regions, ", ")))
	}
	return &Footprint{Report: rep, Details: c.details, Incomplete: sortedKeys(s.Incomplete), AllRegions: s.AllRegions}
}

type classifier struct {
	s       *Snapshot
	now     time.Time
	grace   time.Duration
	details map[string]Detail

	roleARN      map[string]string   // role name -> ARN
	profileRoles map[string][]string // profile ARN -> role names
	profileByARN map[string]Profile
	queueByARN   map[string]*Queue
	usersOfProf  map[string][]string // profile ARN -> live instance IDs
	workers      map[string][]string // region/queue name -> instance IDs
	grantIDs     map[string]bool     // InlinePolicyID of every listed grant
	queueClass   map[string]footprint.Class
	grantClass   map[string]footprint.Class // InlinePolicyID -> class
	roleClass    map[string]footprint.Class
}

func (c *classifier) partition() string {
	if c.s.Partition != "" {
		return c.s.Partition
	}
	return "aws"
}

func (c *classifier) index() {
	s := c.s
	c.roleARN = map[string]string{}
	for _, r := range s.Roles {
		c.roleARN[r.Name] = r.ARN
	}
	c.profileRoles, c.profileByARN = map[string][]string{}, map[string]Profile{}
	for _, p := range s.Profiles {
		c.profileRoles[p.ARN] = p.Roles
		c.profileByARN[p.ARN] = p
	}
	c.queueByARN = map[string]*Queue{}
	for i := range s.Queues {
		c.queueByARN[s.Queues[i].ARN] = &s.Queues[i]
	}
	c.usersOfProf, c.workers = map[string][]string{}, map[string][]string{}
	for _, in := range s.Instances {
		if in.ProfileARN != "" {
			c.usersOfProf[in.ProfileARN] = append(c.usersOfProf[in.ProfileARN], in.ID)
		}
		if m, ok := in.Tags[poolModelTag]; ok && m != "" {
			k := in.Region + "/" + PoolQueueName(m)
			c.workers[k] = append(c.workers[k], in.ID)
		}
		if r, ok := in.Tags[fleetRunTag]; ok && r != "" {
			k := in.Region + "/" + RunQueueName(r)
			c.workers[k] = append(c.workers[k], in.ID)
		}
	}
	c.grantIDs = map[string]bool{}
	for _, r := range s.Roles {
		for _, g := range r.Grants {
			c.grantIDs[InlinePolicyID(r.ARN, g.Policy)] = true
		}
	}
	c.queueClass, c.grantClass, c.roleClass = map[string]footprint.Class{}, map[string]footprint.Class{}, map[string]footprint.Class{}
}

func (c *classifier) young(t map[string]string, fallback time.Time) bool {
	created, ok := tags.CreatedAt(t)
	if !ok {
		created = fallback
	}
	return !created.IsZero() && c.now.Sub(created) < c.grace
}

func (c *classifier) graceText() string {
	return fmt.Sprintf("created within the %s grace", c.grace)
}

// instancesUnseen says why an instance using a global resource could have been
// missed, or "" if every region's instances were listed.
func (c *classifier) instancesUnseen() string {
	if !c.s.AllRegions {
		return "not every region was listed for instances that could use it (use --all-regions)"
	}
	for _, r := range c.s.Regions {
		if c.s.Incomplete["ec2:"+r] {
			return "the instances in " + r + " could not be listed"
		}
	}
	return ""
}

func ownership(t map[string]string) footprint.Ownership {
	switch v, ok := t[tags.ManagedBy]; {
	case ok && v == string(Tool):
		return footprint.OwnershipTagged
	case ok && v != "":
		return footprint.OwnershipForeign
	}
	return footprint.OwnershipLegacyName
}

func (c *classifier) add(out *[]footprint.Resource, r footprint.Resource, d Detail) {
	r.Scope = r.Tags[tags.Scope]
	if r.Ownership == "" {
		r.Ownership = ownership(r.Tags)
	}
	r.Deletable, r.Reason = footprint.Deletable(r, Tool, Patterns())
	*out = append(*out, r)
	c.details[r.ARN] = d
}

func (c *classifier) resources() []footprint.Resource {
	s := c.s
	var out []footprint.Resource

	// Queues first: their classes decide their grants', which decide their
	// roles'.
	var queues []footprint.Resource
	var queueDetails []Detail
	for _, q := range s.Queues {
		ws := c.workers[q.Region+"/"+q.Name]
		d := Detail{Queue: &QueueDetail{Visible: q.Visible, InFlight: q.InFlight, Delayed: q.Delayed, Workers: nonNil(ws)}}
		class := footprint.ClassLitter
		switch {
		case len(ws) > 0:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("%d live worker(s)", len(ws))
		case q.Visible+q.InFlight+q.Delayed > 0:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("%d message(s) queued or in flight", q.Visible+q.InFlight+q.Delayed)
		case s.Incomplete["ec2:"+q.Region]:
			class, d.Why = footprint.ClassActive, "the instances in "+q.Region+" could not be listed, so its workers are unknown"
		case c.young(q.Tags, q.Created):
			class, d.Why = footprint.ClassActive, c.graceText()
		default:
			d.Why = "idle: no workers and no messages"
		}
		c.queueClass[q.ARN] = class
		r := footprint.Resource{ARN: q.ARN, Type: TypeQueue, Region: q.Region, Name: q.Name, Class: class, Tags: q.Tags}
		if role := workerRoleForQueue(q.Name); role != "" {
			if roleARN, ok := c.roleARN[role]; ok {
				if id := InlinePolicyID(roleARN, QueueGrantPolicyName(q.ARN)); c.grantIDs[id] {
					r.DependsOn = append(r.DependsOn, id)
				}
				r.DependsOn = append(r.DependsOn, roleARN)
			}
		}
		queues = append(queues, r)
		queueDetails = append(queueDetails, d)
	}

	// Grants.
	type grantRes struct {
		r footprint.Resource
		d Detail
	}
	grantsByRole := map[string][]grantRes{}
	for _, role := range s.Roles {
		for _, g := range role.Grants {
			id := InlinePolicyID(role.ARN, g.Policy)
			d := Detail{Grants: g.QueueARN}
			class := footprint.ClassLitter
			region := arnRegion(g.QueueARN)
			switch {
			case g.QueueARN == "":
				class, d.Why = footprint.ClassActive, "could not tell which queue it grants"
			case c.queueClass[g.QueueARN] != "":
				class = c.queueClass[g.QueueARN]
				d.Why = "follows its queue, which is " + string(class)
			case !slices.Contains(s.Regions, region):
				class, d.Why = footprint.ClassActive, "its queue is in "+region+", which was not listed"
			case s.Incomplete["sqs:"+region]:
				class, d.Why = footprint.ClassActive, "the queues in "+region+" could not be listed"
			default:
				d.Why = "its queue no longer exists"
			}
			c.grantClass[id] = class
			grantsByRole[role.Name] = append(grantsByRole[role.Name], grantRes{
				r: footprint.Resource{ARN: id, Type: TypeRolePolicy, Region: RegionGlobal, Name: g.Policy, Class: class,
					Ownership: ownership(role.Tags), Tags: role.Tags, DependsOn: []string{role.ARN}},
				d: d,
			})
		}
	}

	// Roles.
	unseen := c.instancesUnseen()
	for _, role := range s.Roles {
		var users []string
		for profARN, roles := range c.profileRoles {
			if slices.Contains(roles, role.Name) {
				users = append(users, c.usersOfProf[profARN]...)
			}
		}
		// An instance whose profile was not listed (it is not named like a
		// calque role) but is named like this role: spawn names them alike.
		for profARN, ids := range c.usersOfProf {
			if _, listed := c.profileByARN[profARN]; !listed && arnName(profARN) == role.Name {
				users = append(users, ids...)
			}
		}
		sort.Strings(users)
		d := Detail{UsedBy: users, ManagedPolicies: role.ManagedPolicies}
		class := footprint.ClassLitter
		activeGrant := ""
		for _, g := range grantsByRole[role.Name] {
			if g.r.Class == footprint.ClassActive {
				activeGrant = g.r.Name
				break
			}
		}
		switch {
		case len(users) > 0:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("used by %d live instance(s)", len(users))
		case activeGrant != "":
			class, d.Why = footprint.ClassActive, "its queue grant "+activeGrant+" is active"
		case unseen != "":
			class, d.Why = footprint.ClassActive, unseen
		case c.young(role.Tags, role.Created):
			class, d.Why = footprint.ClassActive, c.graceText()
		default:
			d.Why = "no live instance uses it"
		}
		c.roleClass[role.Name] = class
		c.add(&out, footprint.Resource{ARN: role.ARN, Type: TypeRole, Region: RegionGlobal, Name: role.Name, Class: class, Tags: role.Tags}, d)
		for _, g := range grantsByRole[role.Name] {
			c.add(&out, g.r, g.d)
		}
	}

	// Instance profiles.
	for _, p := range s.Profiles {
		users := slices.Clone(c.usersOfProf[p.ARN])
		sort.Strings(users)
		d := Detail{UsedBy: users}
		class := footprint.ClassLitter
		activeRole := ""
		var deps []string
		for _, rn := range p.Roles {
			if c.roleClass[rn] == footprint.ClassActive && activeRole == "" {
				activeRole = rn
			}
			if a, ok := c.roleARN[rn]; ok {
				deps = append(deps, a)
			}
		}
		switch {
		case len(users) > 0:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("used by %d live instance(s)", len(users))
		case activeRole != "":
			class, d.Why = footprint.ClassActive, "its role "+activeRole+" is active"
		case unseen != "":
			class, d.Why = footprint.ClassActive, unseen
		case c.young(p.Tags, p.Created):
			class, d.Why = footprint.ClassActive, c.graceText()
		default:
			d.Why = "no live instance uses it"
		}
		c.add(&out, footprint.Resource{ARN: p.ARN, Type: TypeInstanceProfile, Region: RegionGlobal, Name: p.Name, Class: class, Tags: p.Tags, DependsOn: deps}, d)
	}

	for i := range queues {
		c.add(&out, queues[i], queueDetails[i])
	}

	// Security groups.
	for _, g := range s.SecurityGroups {
		d := Detail{ENIs: g.ENIs}
		class := footprint.ClassLitter
		switch {
		case len(g.ENIs) > 0:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("%d network interface(s) use it", len(g.ENIs))
		case s.Incomplete["eni:"+g.Region]:
			class, d.Why = footprint.ClassActive, "its network interfaces could not be listed"
		case c.young(g.Tags, time.Time{}):
			class, d.Why = footprint.ClassActive, c.graceText()
		default:
			d.Why = "no network interface uses it"
		}
		arn := fmt.Sprintf("arn:%s:ec2:%s:%s:security-group/%s", c.partition(), g.Region, s.Account, g.ID)
		c.add(&out, footprint.Resource{ARN: arn, Type: TypeSecurityGroup, Region: g.Region, Name: g.Name, Class: class, Tags: g.Tags}, d)
	}

	// Baked AMIs and their snapshots: the user's.
	for _, im := range s.Images {
		var deps []string
		for _, sn := range im.Snapshots {
			deps = append(deps, c.snapshotARN(im.Region, sn))
		}
		c.add(&out, footprint.Resource{ARN: c.imageARN(im.Region, im.ID), Type: TypeImage, Region: im.Region, Name: im.ID,
			Class: footprint.ClassUserData, Tags: im.Tags, DependsOn: deps},
			Detail{Why: "a baked AMI the user built; delete it with `calque ami delete`"})
	}
	for _, sn := range s.Snapshots {
		c.add(&out, footprint.Resource{ARN: c.snapshotARN(sn.Region, sn.ID), Type: TypeSnapshot, Region: sn.Region, Name: sn.ID,
			Class: footprint.ClassUserData, Tags: sn.Tags},
			Detail{Why: "a baked AMI's snapshot; `calque ami delete` removes it with the AMI"})
	}

	// Instances: always active. Teardown never terminates them.
	for _, in := range s.Instances {
		r := footprint.Resource{ARN: fmt.Sprintf("arn:%s:ec2:%s:%s:instance/%s", c.partition(), in.Region, s.Account, in.ID),
			Type: TypeInstance, Region: in.Region, Name: in.ID, Class: footprint.ClassActive, Tags: in.Tags}
		if _, ok := c.profileByARN[in.ProfileARN]; ok {
			r.DependsOn = []string{in.ProfileARN}
		}
		c.add(&out, r, Detail{Why: "instance is " + in.State})
	}

	// S3 objects.
	for _, o := range s.Objects {
		d := Detail{}
		class := footprint.ClassLitter
		switch {
		case isResultKey(o.Key):
			class, d.Why = footprint.ClassUserData, "a run's results or summary: the user's output"
		case c.now.Sub(o.LastModified) < c.grace:
			class, d.Why = footprint.ClassActive, fmt.Sprintf("written within the %s grace", c.grace)
		default:
			d.Why = fmt.Sprintf("run staging older than the %s grace", c.grace)
		}
		region := o.Region
		if region == "" {
			region = RegionGlobal
		}
		c.add(&out, footprint.Resource{ARN: fmt.Sprintf("arn:%s:s3:::%s/%s", c.partition(), o.Bucket, o.Key), Type: TypeObject,
			Region: region, Name: o.Key, Class: class, Tags: o.Tags}, d)
	}
	return out
}

func (c *classifier) imageARN(region, id string) string {
	return fmt.Sprintf("arn:%s:ec2:%s::image/%s", c.partition(), region, id)
}

func (c *classifier) snapshotARN(region, id string) string {
	return fmt.Sprintf("arn:%s:ec2:%s::snapshot/%s", c.partition(), region, id)
}

// arnRegion is the region field of an ARN, or "".
func arnRegion(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[3]
}

// arnName is the last path segment of an ARN's resource.
func arnName(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Idle reports whether the footprint shows calque idle in the account, and
// if not, why. calque is idle when no resource is active, every listing
// succeeded, and every region was listed. User data and litter are not
// activity.
func (f *Footprint) Idle() (bool, string) {
	if len(f.Incomplete) > 0 {
		return false, "listings failed: " + strings.Join(f.Incomplete, ", ")
	}
	if !f.AllRegions {
		return false, "not every region was listed"
	}
	for _, res := range f.Report.Resources {
		if res.Class == footprint.ClassActive {
			return false, res.Type + " " + res.Name + " is active"
		}
	}
	return true, ""
}
