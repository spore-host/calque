package calquetags

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"

	"github.com/spore-host/libs/tags/iamtags"
)

// Warn is where tagging warnings go. A variable so tests can capture it.
var Warn io.Writer = os.Stderr

// IAMAPI is the slice of the IAM SDK EnsureRoleTags needs. A real *iam.Client
// satisfies it.
type IAMAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	TagRole(ctx context.Context, in *iam.TagRoleInput, opts ...func(*iam.Options)) (*iam.TagRoleOutput, error)
	TagInstanceProfile(ctx context.Context, in *iam.TagInstanceProfileInput, opts ...func(*iam.Options)) (*iam.TagInstanceProfileOutput, error)
}

// EnsureRoleTags brings the IAM role name, and the instance profile of the
// same name (spawn's CreateOrGetInstanceProfile names them alike), up to want.
//
// spawn's CreateOrGetInstanceProfile tags a role only when it creates it, so a
// role an older calque created, which carries only spawn's spawn:* tags, would
// otherwise stay invisible to cleanup forever. It is called after every
// CreateOrGetInstanceProfile, new role or not, and writes only what is missing
// or wrong (see Converge), so the common case is two reads and no write. It
// also makes calque's managed-by win if spawn ever stamps its own on create.
//
// Needs iam:TagRole and iam:TagInstanceProfile; the reads use iam:GetRole and
// iam:GetInstanceProfile, which spawn already needs. The error is for the
// caller to warn about: an untagged role is a cleanup gap, not a reason to
// fail a run.
func EnsureRoleTags(ctx context.Context, api IAMAPI, name string, want map[string]string) error {
	var errs []error
	role, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		errs = append(errs, fmt.Errorf("read role %s tags: %w", name, err))
	} else if add := Converge(roleTags(role), want); len(add) > 0 {
		if _, err := api.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(name), Tags: iamtags.FromMap(add)}); err != nil {
			errs = append(errs, fmt.Errorf("tag role %s: %w", name, err))
		}
	}
	prof, err := api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	if err != nil {
		errs = append(errs, fmt.Errorf("read instance profile %s tags: %w", name, err))
	} else if add := Converge(profileTags(prof), want); len(add) > 0 {
		if _, err := api.TagInstanceProfile(ctx, &iam.TagInstanceProfileInput{InstanceProfileName: aws.String(name), Tags: iamtags.FromMap(add)}); err != nil {
			errs = append(errs, fmt.Errorf("tag instance profile %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func roleTags(out *iam.GetRoleOutput) map[string]string {
	if out == nil || out.Role == nil {
		return nil
	}
	return iamtags.ToMap(out.Role.Tags)
}

func profileTags(out *iam.GetInstanceProfileOutput) map[string]string {
	if out == nil || out.InstanceProfile == nil {
		return nil
	}
	return iamtags.ToMap(out.InstanceProfile.Tags)
}

// WarnRoleTags runs EnsureRoleTags and writes any error to Warn.
func WarnRoleTags(ctx context.Context, api IAMAPI, name string, want map[string]string) {
	if err := EnsureRoleTags(ctx, api, name, want); err != nil {
		_, _ = fmt.Fprintf(Warn, "[tags] WARNING: could not tag IAM role/instance profile %s (needs iam:TagRole and iam:TagInstanceProfile); "+
			"it works, but calque's cleanup will not recognise it: %v\n", name, err)
	}
}

// PutObjectAPI is the one S3 call PutObject needs.
type PutObjectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

var (
	objectTaggingDenied atomic.Bool
	objectTaggingWarn   sync.Once
)

// PutObject sends in with its Tagging set to tagging (see ObjectTagging).
//
// Tagging an object at PutObject needs s3:PutObjectTagging as well as
// s3:PutObject. calque grants it to the instance roles it manages, but a
// pool worker still on a grant from an older calque, or a caller whose own
// policy predates calque#227, lacks it. S3 then refuses the WHOLE write with
// AccessDenied. Rather than fail a run over a tag, PutObject resends the
// object untagged, warns once, and stops asking for tags for the rest of the
// process. The resend needs the body to be rewindable (every calque caller
// passes a file or an in-memory reader); one that isn't gets the original
// error.
func PutObject(ctx context.Context, c PutObjectAPI, in *s3.PutObjectInput, tagging string) (*s3.PutObjectOutput, error) {
	if tagging == "" || objectTaggingDenied.Load() {
		return c.PutObject(ctx, in)
	}
	tagged := *in
	tagged.Tagging = aws.String(tagging)
	out, err := c.PutObject(ctx, &tagged)
	if err == nil || !IsAccessDenied(err) {
		return out, err
	}
	if in.Body != nil {
		s, ok := in.Body.(io.Seeker)
		if !ok {
			return out, err
		}
		if _, serr := s.Seek(0, io.SeekStart); serr != nil {
			return out, err
		}
	}
	out2, err2 := c.PutObject(ctx, in)
	if err2 != nil {
		return out2, err2 // not the tagging: the untagged write is refused too
	}
	objectTaggingDenied.Store(true)
	objectTaggingWarn.Do(func() {
		_, _ = fmt.Fprintf(Warn, "[tags] WARNING: s3://%s refused a tagged PutObject (AccessDenied; s3:PutObjectTagging is missing), "+
			"so calque is writing this run's objects untagged. A lifecycle rule keyed on %s will not expire them: %v\n",
			aws.ToString(in.Bucket), "spore-host:managed-by=calque", err)
	})
	return out2, nil
}

// IsAccessDenied reports whether err is an AccessDenied refusal, in any
// service's spelling of it (AccessDenied, AccessDeniedException,
// AWS.SimpleQueueService.AccessDenied, ...).
func IsAccessDenied(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && strings.Contains(apiErr.ErrorCode(), "AccessDenied")
}
