package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallScriptPlatformsAndTransactionalFailure(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"GC_DOLT_VERSION", "GC_DOLT_SHA256", "GC_BD_VERSION", "GC_BD_SHA256"} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("installer missing required release-tuple input %s", required)
		}
	}
	missingPins := exec.Command("bash", filepath.Join(root, "install.sh"), "1.3.7")
	missingPins.Env = append(os.Environ(), "GC_DOLT_VERSION=", "GC_DOLT_SHA256=", "GC_BD_VERSION=", "GC_BD_SHA256=")
	if out, err := missingPins.CombinedOutput(); err == nil || !strings.Contains(string(out), "GC_DOLT_VERSION is required") {
		t.Fatalf("installer did not fail closed without a release tuple: %v\n%s", err, out)
	}
	for _, tc := range []struct{ os, machine string }{{"Linux", "x86_64"}, {"Linux", "aarch64"}, {"Darwin", "x86_64"}, {"Darwin", "arm64"}} {
		t.Run(tc.os+"/"+tc.machine, func(t *testing.T) {
			dir := runInstallFixture(t, tc.os, tc.machine, "")
			for _, name := range []string{"gc", "dolt", "bd"} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !strings.Contains(string(got), name) {
					t.Fatalf("%s install = %q, %v", name, got, err)
				}
			}
		})
	}
	for _, failure := range []string{"checksum", "final"} {
		t.Run(failure, func(t *testing.T) {
			dir := runInstallFixture(t, "Linux", "x86_64", failure)
			for _, name := range []string{"gc", "dolt", "bd"} {
				got, _ := os.ReadFile(filepath.Join(dir, name))
				if string(got) != "old-"+name {
					t.Fatalf("%s was not preserved: %q", name, got)
				}
			}
		})
	}
}

func runInstallFixture(t *testing.T, goos, machine, failure string) string {
	t.Helper()
	root, tmp := repoRoot(t), t.TempDir()
	fixtures, fake, dir := filepath.Join(tmp, "fixtures"), filepath.Join(tmp, "bin"), filepath.Join(tmp, "install")
	for _, p := range []string{fixtures, fake, dir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"gc", "dolt", "bd"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	osname := strings.ToLower(goos)
	gc := "gascity_1.3.7_" + osname + "_" + arch + ".tar.gz"
	doltVersion, bdVersion := "2.1.10", "v1.2.3"
	dolt := "dolt-" + osname + "-" + arch + ".tar.gz"
	bd := "beads_" + strings.TrimPrefix(bdVersion, "v") + "_" + osname + "_" + arch + ".tar.gz"
	writeArchive(t, fixtures, gc, "gc", "gc")
	writeArchive(t, fixtures, dolt, "dolt-"+osname+"-"+arch+"/bin/dolt", "dolt")
	writeArchive(t, fixtures, bd, "bd", "bd")
	data, err := os.ReadFile(filepath.Join(fixtures, gc))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "gascity_1.3.7_checksums.txt"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), gc)), 0o644); err != nil {
		t.Fatal(err)
	}
	doltData, err := os.ReadFile(filepath.Join(fixtures, dolt))
	if err != nil {
		t.Fatal(err)
	}
	bdData, err := os.ReadFile(filepath.Join(fixtures, bd))
	if err != nil {
		t.Fatal(err)
	}
	writeShim(t, filepath.Join(fake, "uname"), "#!/bin/sh\nif [ \"$1\" = -s ]; then echo '"+goos+"'; else echo '"+machine+"'; fi\n")
	writeShim(t, filepath.Join(fake, "curl"), "#!/bin/sh\nout=\nurl=\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then out=$2; shift 2; else url=$1; shift; fi; done\nname=${url##*/}\nif [ \"${FAILURE:-}\" = checksum ] && echo \"$name\" | grep -q '^gascity_.*tar.gz$'; then printf corrupt > \"$out\"; else cp \"$FIXTURES/$name\" \"$out\"; fi\n")
	if failure == "final" {
		writeShim(t, filepath.Join(fake, "install"), "#!/bin/sh\nfor last; do :; done\ncase \"$last\" in */.bd.tmp.*) exit 73;; esac\nexec \"$REAL_INSTALL\" \"$@\"\n")
	}
	cmd := exec.Command("bash", filepath.Join(root, "install.sh"), "1.3.7")
	cmd.Env = append(os.Environ(), "PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURES="+fixtures, "FAILURE="+failure, "GC_INSTALL_DIR="+dir, "GC_INSTALL_REPO=example/gascity", "GC_DOLT_VERSION="+doltVersion, fmt.Sprintf("GC_DOLT_SHA256=%x", sha256.Sum256(doltData)), "GC_BD_VERSION="+bdVersion, fmt.Sprintf("GC_BD_SHA256=%x", sha256.Sum256(bdData)), "REAL_INSTALL=/usr/bin/install")
	out, err := cmd.CombinedOutput()
	if failure == "" && err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if failure != "" && err == nil {
		t.Fatalf("installer unexpectedly succeeded: %s", out)
	}
	return dir
}

func writeArchive(t *testing.T, fixtures, name, entry, label string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+label+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "-czf", filepath.Join(fixtures, name), "-C", root, strings.Split(entry, "/")[0])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
}

func writeShim(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
