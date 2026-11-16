package calquefootprint

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDependencies keeps the package importable by spawn's ttl-reaper
// (spore-host/spawn#1654): it may depend on the standard library, the AWS SDK,
// smithy-go, spore-host/libs and calque's own tag helpers, and nothing else.
// In particular not spawn (the reaper is spawn's), lagotto, truffle, cohort,
// or any other calque package, which pull those in.
func TestDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	allowed := []string{
		"github.com/aws/aws-sdk-go-v2",
		"github.com/aws/smithy-go",
		"github.com/spore-host/libs/",
		"github.com/spore-host/calque/pkg/calquefootprint",
		"github.com/spore-host/calque/internal/calquetags",
	}
	for _, p := range strings.Fields(string(out)) {
		ok := false
		for _, a := range allowed {
			ok = ok || strings.HasPrefix(p, a)
		}
		if !ok {
			t.Errorf("calquefootprint depends on %s; the reaper can only take the AWS SDK and spore-host/libs", p)
		}
	}
}
