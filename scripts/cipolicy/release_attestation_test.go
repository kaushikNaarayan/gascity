package cipolicy

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseAttestationIsTemporaryNonBlockingAndPreservesSBOM(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		t.Fatal("release workflow has no jobs")
	}
	attest, ok := jobs["attest-release"].(map[string]any)
	if !ok {
		t.Fatal("release workflow has no attest-release job")
	}
	if nonBlocking, ok := attest["continue-on-error"].(bool); !ok || !nonBlocking {
		t.Fatal("attest-release must remain temporary non-blocking until gcf-69bt resolves the policy")
	}
	steps, ok := attest["steps"].([]any)
	if !ok {
		t.Fatal("attest-release has no steps")
	}
	var hasSBOM, hasUpload bool
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if step["name"] == "Generate release SBOM" {
			hasSBOM = true
		}
		if step["name"] == "Upload release SBOM" {
			hasUpload = true
		}
	}
	if !hasSBOM || !hasUpload {
		t.Fatalf("SBOM generation/upload must remain enabled (generate=%t upload=%t)", hasSBOM, hasUpload)
	}

	homebrew, ok := jobs["update-homebrew-formula"].(map[string]any)
	if !ok {
		t.Fatal("release workflow has no update-homebrew-formula job")
	}
	if homebrew["needs"] != "release" {
		t.Fatalf("update-homebrew-formula must depend only on release, got %#v", homebrew["needs"])
	}
}
