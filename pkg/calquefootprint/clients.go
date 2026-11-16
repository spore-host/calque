package calquefootprint

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// ClientsFromConfig returns Clients built from cfg: one IAM client, an SQS and
// an EC2 client per region (made on first use), and an S3 client per bucket in
// the bucket's own region, found with manager.GetBucketRegion from cfg.Region.
func ClientsFromConfig(cfg aws.Config) Clients {
	var mu sync.Mutex
	sqsBy, ec2By := map[string]*sqs.Client{}, map[string]*ec2.Client{}
	type bucketClient struct {
		c      *s3.Client
		region string
	}
	s3By := map[string]bucketClient{}
	regional := func(region string) aws.Config {
		c := cfg.Copy()
		c.Region = region
		return c
	}
	return Clients{
		IAM: iam.NewFromConfig(cfg),
		SQS: func(region string) (SQSAPI, error) {
			mu.Lock()
			defer mu.Unlock()
			if c, ok := sqsBy[region]; ok {
				return c, nil
			}
			c := sqs.NewFromConfig(regional(region))
			sqsBy[region] = c
			return c, nil
		},
		EC2: func(region string) (EC2API, error) {
			mu.Lock()
			defer mu.Unlock()
			if c, ok := ec2By[region]; ok {
				return c, nil
			}
			c := ec2.NewFromConfig(regional(region))
			ec2By[region] = c
			return c, nil
		},
		S3: func(ctx context.Context, bucket string) (S3API, string, error) {
			mu.Lock()
			if bc, ok := s3By[bucket]; ok {
				mu.Unlock()
				return bc.c, bc.region, nil
			}
			mu.Unlock()
			region, err := manager.GetBucketRegion(ctx, s3.NewFromConfig(cfg), bucket)
			if err != nil {
				return nil, "", fmt.Errorf("find bucket %s's region: %w", bucket, err)
			}
			if region == "" {
				region = cfg.Region
			}
			c := s3.NewFromConfig(regional(region))
			mu.Lock()
			s3By[bucket] = bucketClient{c: c, region: region}
			mu.Unlock()
			return c, region, nil
		},
	}
}

// RegionsAPI is the one EC2 call EnabledRegions makes.
type RegionsAPI interface {
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, opts ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
}

// EnabledRegions returns the regions enabled in the account, sorted: those
// to pass as Options.Regions with AllRegions, and as DeleterOptions.Regions.
func EnabledRegions(ctx context.Context, api RegionsAPI) ([]string, error) {
	out, err := api.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, fmt.Errorf("describe regions: %w", err)
	}
	var regions []string
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	sort.Strings(regions)
	return regions, nil
}
