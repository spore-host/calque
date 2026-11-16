// Package awstest answers AWS SDK v2 calls in-process, so unit tests can drive
// real SDK clients, and code that builds its own clients from an aws.Config
// (spawn's pkg/aws), without credentials or a network.
//
// It exists for calque#227: tags/tagtest's Detector is SDK middleware, so it
// only sees calls made through a real SDK client. An interface fake bypasses
// it. Config's clients run the real middleware stack up to the end of the
// Initialize step (service and operation names recorded, input validated,
// tagtest's check run) and then return the Handler's answer instead of
// serializing, signing and sending.
//
// Test-only by intent: nothing outside _test.go files imports it.
package awstest

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// Handler answers one operation. It must return the operation's own output
// type (for example *sqs.CreateQueueOutput) or an error.
type Handler func(operation string, input any) (output any, err error)

// Services maps an SDK service ID ("IAM", "SQS", "S3", "EC2", "STS", ...) to
// its Handler.
type Services map[string]Handler

// MiddlewareID is the ID of the answering middleware.
const MiddlewareID = "calque/awstest"

// Config returns an aws.Config, region us-east-1, whose operations are
// answered by svcs. A call to a service with no Handler fails with an error
// naming it, so a test can't silently pass on a call it didn't model.
//
// Install a tagtest Detector on the returned config (Detector.Install) to
// check create calls: the Detector sits right after the SDK's service
// metadata middleware, before this one at the end of Initialize.
func Config(svcs Services) aws.Config {
	cfg := aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}}
	cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(MiddlewareID, func(ctx context.Context, in middleware.InitializeInput, _ middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
			service, op := middleware.GetServiceID(ctx), middleware.GetOperationName(ctx)
			if service == "" {
				service = awsmiddleware.GetServiceID(ctx)
			}
			if op == "" {
				op = awsmiddleware.GetOperationName(ctx)
			}
			h, ok := svcs[service]
			if !ok {
				return middleware.InitializeOutput{}, middleware.Metadata{}, fmt.Errorf("awstest: no handler for service %q (operation %s)", service, op)
			}
			out, err := h(op, in.Parameters)
			return middleware.InitializeOutput{Result: out}, middleware.Metadata{}, err
		}), middleware.After)
	})
	return cfg
}

// Unhandled is the error a Handler returns for an operation it does not model.
func Unhandled(service, op string) error {
	return fmt.Errorf("awstest: %s %s is not modelled by this test", service, op)
}

// APIError is a smithy.APIError with a fixed code, for a Handler to return
// (for example "AccessDenied" or "NoSuchEntity").
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// ErrorCode implements smithy.APIError.
func (e *APIError) ErrorCode() string { return e.Code }

// ErrorMessage implements smithy.APIError.
func (e *APIError) ErrorMessage() string { return e.Message }

// ErrorFault implements smithy.APIError.
func (e *APIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

var _ smithy.APIError = (*APIError)(nil)
