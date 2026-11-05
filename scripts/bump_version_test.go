package scripts_test

import "testing"

// TestBumpVersionCarriesVersionsManifestInTaggedCommit runs
// scripts/test-bump-version.sh (gcf-g2czv): the release commit bump-version.sh
// tags must carry VERSIONS GC_VERSION=<new>, because install.sh reads VERSIONS
// at the tag and refuses a release whose manifest names another gc version.
func TestBumpVersionCarriesVersionsManifestInTaggedCommit(t *testing.T) {
	out, err := scriptCommand(repoRoot(t), "test-bump-version.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("test-bump-version.sh failed: %v\n%s", err, out)
	}
}
