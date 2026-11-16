package calquetags

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/spore-host/calque/internal/awstest"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/tagtest"
)

var t0 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func TestFor_SporeHostSetPlusCalqueManaged(t *testing.T) {
	got := For("pool:llama", t0)
	want := map[string]string{
		tags.ManagedBy: "calque",
		tags.Scope:     "pool:llama",
		tags.Created:   "2026-11-16T10:00:00Z",
		tags.Schema:    tags.SchemaVersion,
		Managed:        "true",
	}
	if len(got) != len(want) {
		t.Errorf("For = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("For[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestForRun_EmptyRunIDHasNoScopeOrRunID(t *testing.T) {
	got := ForRun("", t0)
	for _, k := range []string{tags.Scope, RunID} {
		if _, ok := got[k]; ok {
			t.Errorf("ForRun(\"\")[%q] present, want absent", k)
		}
	}
	if !tags.IsManagedBy(got, tags.ToolCalque) {
		t.Errorf("ForRun(\"\") = %v, want managed-by=calque", got)
	}
	r := ForRun("r1", t0)
	if r[tags.Scope] != "run:r1" || r[RunID] != "r1" {
		t.Errorf("ForRun(r1) = %v, want scope run:r1 and calque:run-id r1", r)
	}
}

func TestValue_ReplacesIllegalCharactersAndCaps(t *testing.T) {
	if got := Value("meta-llama/Llama-3.1 8B@sha256:ab+c=d_e"); got != "meta-llama/Llama-3.1 8B@sha256:ab+c=d_e" {
		t.Errorf("legal value changed: %q", got)
	}
	if got := Value(`run#1;"x"`); got != "run_1__x_" {
		t.Errorf("Value = %q, want illegal characters replaced", got)
	}
	if got := Value(strings.Repeat("a", 300)); len(got) != 256 {
		t.Errorf("len(Value(300 chars)) = %d, want 256", len(got))
	}
}

func TestConverge_KeepsFirstCreatedAndFixesTheRest(t *testing.T) {
	want := For("pool:m", t0)
	// An untagged resource (an older calque's) gets the whole set.
	if got := Converge(nil, want); len(got) != len(want) {
		t.Errorf("Converge(nil) = %v, want all of %v", got, want)
	}
	// An already-tagged resource gets nothing, even though want's created is newer.
	have := For("pool:m", t0.Add(-48*time.Hour))
	if got := Converge(have, want); len(got) != 0 {
		t.Errorf("Converge(tagged) = %v, want nothing (created is never moved)", got)
	}
	// A wrong managed-by (spawn stamping its own on create) is corrected.
	have[tags.ManagedBy] = "spawn"
	if got := Converge(have, want); len(got) != 1 || got[tags.ManagedBy] != "calque" {
		t.Errorf("Converge(managed-by=spawn) = %v, want only managed-by=calque", got)
	}
}

func TestObjectTagging(t *testing.T) {
	q, err := url.ParseQuery(ObjectTagging("run-7"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{tags.ManagedBy: "calque", RunID: "run-7", tags.Scope: "run:run-7", Managed: "true"} {
		if q.Get(k) != v {
			t.Errorf("ObjectTagging[%q] = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) > 10 {
		t.Errorf("ObjectTagging has %d tags; S3 allows 10 per object", len(q))
	}
	if q, _ := url.ParseQuery(ObjectTagging("")); q.Has(RunID) || q.Get(tags.ManagedBy) != "calque" {
		t.Errorf("ObjectTagging(\"\") = %v, want managed-by and no run id", q)
	}
}

// fakePut records PutObject calls; deny makes a tagged call fail AccessDenied.
type fakePut struct {
	mu     sync.Mutex
	deny   bool
	bodies []string
	tagged []bool
}

func (f *fakePut) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(in.Body)
	f.bodies = append(f.bodies, string(b))
	f.tagged = append(f.tagged, in.Tagging != nil)
	if f.deny && in.Tagging != nil {
		return nil, &awstest.APIError{Code: "AccessDenied", Message: "s3:PutObjectTagging"}
	}
	return &s3.PutObjectOutput{}, nil
}

func resetObjectTagging(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := Warn
	Warn = &buf
	objectTaggingDenied.Store(false)
	objectTaggingWarn = sync.Once{}
	t.Cleanup(func() {
		Warn = prev
		objectTaggingDenied.Store(false)
		objectTaggingWarn = sync.Once{}
	})
	return &buf
}

// TestPutObject_TagsAndPassesTagtest drives a real S3 client through
// tagtest: the object goes out with managed-by=calque in its Tagging.
func TestPutObject_TagsAndPassesTagtest(t *testing.T) {
	resetObjectTagging(t)
	var got *s3.PutObjectInput
	cfg := awstest.Config(awstest.Services{"S3": func(op string, in any) (any, error) {
		got = in.(*s3.PutObjectInput)
		return &s3.PutObjectOutput{}, nil
	}})
	tagtest.New(t, tags.ToolCalque).Install(&cfg)
	_, err := PutObject(context.Background(), s3.NewFromConfig(cfg), &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("runs/r/x"), Body: strings.NewReader("x"),
	}, ObjectTagging("r"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !strings.Contains(aws.ToString(got.Tagging), "calque%3Arun-id=r") {
		t.Errorf("Tagging = %q, want it to carry calque:run-id=r", aws.ToString(got.Tagging))
	}
}

// TestPutObject_AccessDeniedFallsBackUntaggedOnce: without
// s3:PutObjectTagging the write is resent untagged with the same body, a
// warning is printed once, and later writes don't ask for tags again.
func TestPutObject_AccessDeniedFallsBackUntaggedOnce(t *testing.T) {
	warn := resetObjectTagging(t)
	f := &fakePut{deny: true}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := PutObject(ctx, f, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("body")}, ObjectTagging("r")); err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
	}
	if want := []bool{true, false, false}; len(f.tagged) != 3 || f.tagged[0] != want[0] || f.tagged[1] != want[1] || f.tagged[2] != want[2] {
		t.Errorf("tagged per call = %v, want %v (one refused tagged try, then untagged)", f.tagged, want)
	}
	for i, b := range f.bodies {
		if b != "body" {
			t.Errorf("call %d sent body %q, want the full body (rewound for the resend)", i, b)
		}
	}
	if n := strings.Count(warn.String(), "[tags] WARNING"); n != 1 {
		t.Errorf("warned %d times, want once: %q", n, warn.String())
	}
}

// TestPutObject_OtherErrorsAreReturned: a refusal that isn't AccessDenied is
// the caller's, not retried.
func TestPutObject_OtherErrorsAreReturned(t *testing.T) {
	resetObjectTagging(t)
	cfg := awstest.Config(awstest.Services{"S3": func(string, any) (any, error) {
		return nil, &awstest.APIError{Code: "NoSuchBucket"}
	}})
	if _, err := PutObject(context.Background(), s3.NewFromConfig(cfg), &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k")}, ObjectTagging("r")); err == nil {
		t.Fatal("PutObject: want the NoSuchBucket error")
	}
}

// TestEnsureRoleTags_ConvergesAnOlderRole: a role and profile an older
// calque created carry only spawn's tags. EnsureRoleTags adds the calque set
// to both; a second call writes nothing.
func TestEnsureRoleTags_ConvergesAnOlderRole(t *testing.T) {
	f := awstest.NewIAM()
	f.SeedRole("calque-pool-worker", map[string]string{"spawn:managed": "true"})
	cfg := awstest.Config(awstest.Services{"IAM": f.Handle})
	c := iam.NewFromConfig(cfg)
	want := For(ScopePoolWorkers, t0)
	if err := EnsureRoleTags(context.Background(), c, "calque-pool-worker", want); err != nil {
		t.Fatal(err)
	}
	for _, got := range []map[string]string{f.RoleTags("calque-pool-worker"), f.ProfileTags("calque-pool-worker")} {
		if !tags.IsManagedBy(got, tags.ToolCalque) || got["spawn:managed"] != "true" || got[tags.Scope] != ScopePoolWorkers {
			t.Errorf("tags = %v, want calque's set added beside spawn's", got)
		}
	}
	before := len(f.Ops())
	if err := EnsureRoleTags(context.Background(), c, "calque-pool-worker", For(ScopePoolWorkers, t0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.Ops()[before:] {
		if strings.HasPrefix(op, "Tag") {
			t.Errorf("second EnsureRoleTags called %s; want reads only", op)
		}
	}
	if got := f.RoleTags("calque-pool-worker")[tags.Created]; got != "2026-11-16T10:00:00Z" {
		t.Errorf("spore-host:created = %q, want the first stamp kept", got)
	}
}

func TestEnsureRoleTags_ReportsFailure(t *testing.T) {
	cfg := awstest.Config(awstest.Services{"IAM": func(op string, _ any) (any, error) {
		return nil, &awstest.APIError{Code: "AccessDenied", Message: op}
	}})
	if err := EnsureRoleTags(context.Background(), iam.NewFromConfig(cfg), "r", For("", t0)); err == nil || !IsAccessDenied(err) {
		t.Errorf("EnsureRoleTags = %v, want the AccessDenied", err)
	}
}
