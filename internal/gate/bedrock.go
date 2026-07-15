package gate

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
)

// LiveCatalog fetches the Bedrock foundation-model catalog via the AWS API. The
// catalog is region-gated and moves, so we fetch at analysis time and never
// hardcode a snapshot (§11). Region defaults to us-east-1 per the spike decision.
type LiveCatalog struct {
	client *bedrock.Client
}

// NewLiveCatalog builds a catalog client for the given region using the default
// AWS credential chain (AWS_PROFILE, env, shared config, IAM role).
func NewLiveCatalog(ctx context.Context, region string) (*LiveCatalog, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &LiveCatalog{client: bedrock.NewFromConfig(cfg)}, nil
}

// Models implements Catalog by calling ListFoundationModels.
func (c *LiveCatalog) Models(ctx context.Context) ([]CatalogEntry, error) {
	out, err := c.client.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, err
	}
	entries := make([]CatalogEntry, 0, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		e := CatalogEntry{}
		if m.ModelId != nil {
			e.ModelID = *m.ModelId
		}
		if m.ModelName != nil {
			e.ModelName = *m.ModelName
		}
		if m.ProviderName != nil {
			e.Provider = *m.ProviderName
		}
		for _, im := range m.InputModalities {
			e.InputModal = append(e.InputModal, string(im))
		}
		for _, om := range m.OutputModalities {
			e.OutputModal = append(e.OutputModal, string(om))
		}
		for _, rt := range m.InferenceTypesSupported {
			e.ResponseTypes = append(e.ResponseTypes, string(rt))
		}
		entries = append(entries, e)
	}
	return entries, nil
}
