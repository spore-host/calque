package exec

import (
	"context"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AZSubnet pairs an Availability Zone with the default subnet in it. The Acquirer
// sweeps these on capacity failure, and the subnet is passed to the launch so
// spawn doesn't try to place into an AZ with no default subnet (which yields
// InvalidInput, not a capacity signal — observed on us-west-2d for g7e).
type AZSubnet struct {
	AZ     string
	Subnet string
}

// AZsForInstance returns, sorted, the (AZ, default-subnet) pairs where the region
// both OFFERS the instance type AND has a default subnet. These feed the
// Acquirer's AZ sweep — trying each on a capacity failure before backing off (AZ
// breadth within a region is free, mirroring lagotto's launchAcrossAZs).
//
// Filtering to AZs-with-a-default-subnet is load-bearing: spawn, given a placement
// AZ but no SubnetID, needs a default subnet IN that AZ; an AZ without one fails
// with InvalidInput (a config error, not capacity). We pass the subnet explicitly
// to be unambiguous.
func AZsForInstance(ctx context.Context, c *ec2.Client, instanceType string) ([]AZSubnet, error) {
	off, err := c.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-type"), Values: []string{instanceType}},
		},
	})
	if err != nil {
		return nil, err
	}
	offering := map[string]bool{}
	for _, o := range off.InstanceTypeOfferings {
		if o.Location != nil {
			offering[*o.Location] = true
		}
	}

	// Default subnet per AZ.
	subs, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}},
	})
	if err != nil {
		return nil, err
	}
	var out []AZSubnet
	for _, s := range subs.Subnets {
		if s.AvailabilityZone == nil || s.SubnetId == nil {
			continue
		}
		if offering[*s.AvailabilityZone] {
			out = append(out, AZSubnet{AZ: *s.AvailabilityZone, Subnet: *s.SubnetId})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AZ < out[j].AZ })
	return out, nil
}
