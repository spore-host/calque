package exec

import (
	"context"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AZsForInstance returns the Availability Zones in a region that OFFER the given
// instance type, sorted. These feed the Acquirer's AZ sweep — trying each offered
// AZ on a capacity failure before backing off (AZ breadth within a region is free,
// mirroring lagotto's launchAcrossAZs). Offered != has-capacity, but a non-offering
// AZ is a guaranteed miss, so restricting the sweep to offering AZs avoids wasted
// attempts.
func AZsForInstance(ctx context.Context, c *ec2.Client, instanceType string) ([]string, error) {
	out, err := c.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{
			{Name: aws.String("instance-type"), Values: []string{instanceType}},
		},
	})
	if err != nil {
		return nil, err
	}
	var azs []string
	for _, o := range out.InstanceTypeOfferings {
		if o.Location != nil {
			azs = append(azs, *o.Location)
		}
	}
	sort.Strings(azs)
	return azs, nil
}
