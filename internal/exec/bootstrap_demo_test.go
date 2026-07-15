package exec
import ("strings";"testing")
func TestBootstrapCommandShape(t *testing.T){
  c := BootstrapConfig{BaseImage:"vllm/vllm-openai:latest",Bucket:"b",ArtifactPrefix:"runs/x/art",ManifestKey:"runs/x/manifest.json",Region:"us-west-2"}
  cmd := c.Command()
  for _, w := range []string{"--gpus all","aws s3 cp","docker pull vllm/vllm-openai","warmd","--manifest s3://b/runs/x/manifest.json"} {
    if !strings.Contains(cmd, w) { t.Errorf("missing %q in:\n%s", w, cmd) }
  }
}
