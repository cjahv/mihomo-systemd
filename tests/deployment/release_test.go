package deployment

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreAssetSelectionUsesBaselineAndDigest(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "release.json")
	body := `{"assets":[{"name":"mihomo-linux-amd64-v3-v1.2.3.gz","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"name":"mihomo-linux-amd64-v1-v1.2.3.gz","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"name":"mihomo-linux-armv6-v1.2.3.gz"},{"name":"mihomo-linux-armv7-v1.2.3.gz"},{"name":"mihomo-linux-arm64-v1.2.3.gz"},{"name":"mihomo-linux-386-v1.2.3.gz"}]}`
	if err := os.WriteFile(metadata, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ arch, arm, want string }{{"amd64", "5,softfloat", "amd64-v1"}, {"arm", "6,softfloat", "armv6"}, {"arm", "7,softfloat", "armv7"}, {"arm64", "5,softfloat", "arm64"}, {"386", "5,softfloat", "386"}} {
		cmd := exec.Command("bash", "-c", `source "$1"; select_core_asset "$2" v1.2.3 linux "$3" "$4"`, "asset", filepath.Join(repositoryRoot(t), "deploy", "lib", "release.sh"), metadata, tc.arch, tc.arm)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "mihomo-linux-"+tc.want+"-v1.2.3.gz" {
			t.Fatalf("asset=%s err=%v", out, err)
		}
	}
	cmd := exec.Command("bash", "-c", `source "$1"; asset_sha256 "$2" mihomo-linux-amd64-v1-v1.2.3.gz`, "digest", filepath.Join(repositoryRoot(t), "deploy", "lib", "release.sh"), metadata)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != strings.Repeat("b", 64) {
		t.Fatalf("digest=%s err=%v", out, err)
	}
}
func TestCoreAssetRefusesV3OnlyAndMissingDigest(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "release.json")
	writeDeploymentFixture(t, metadata, `{"assets":[{"name":"mihomo-linux-amd64-v3-v1.2.3.gz"}]}`)
	module := filepath.Join(repositoryRoot(t), "deploy", "lib", "release.sh")
	for _, script := range []string{`source "$1"; select_core_asset "$2" v1.2.3 linux amd64 5,softfloat`, `source "$1"; asset_sha256 "$2" mihomo-linux-amd64-v3-v1.2.3.gz`} {
		if err := exec.Command("bash", "-c", script, "asset", module, metadata).Run(); err == nil {
			t.Fatal("accepted unsupported or unverified asset")
		}
	}
}
func TestSHA256MismatchIsRejected(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "artifact")
	writeDeploymentFixture(t, file, "corrupt")
	cmd := exec.Command("bash", "-c", `source "$1"; verify_asset "$2" "$3"`, "checksum", filepath.Join(repositoryRoot(t), "deploy", "lib", "release.sh"), strings.Repeat("a", 64), file)
	if err := cmd.Run(); err == nil {
		t.Fatal("accepted corrupt artifact")
	}
}

func TestCoreReleaseVersion(t *testing.T) {
	for _, tc := range []struct{ banner, want string }{
		{"Mihomo Meta v1.19.32 linux amd64 with go1.26.8\nUse tags: with_gvisor", "v1.19.32"},
		{"Mihomo v1.0.0 linux/amd64", "v1.0.0"},
		{"Mihomo Meta v1.19.32-alpha linux amd64", ""},
		{"Mihomo Meta alpha-g1234 linux amd64", ""},
		{"custom v1.19.32 linux amd64", ""},
		{"Mihomo Meta v1.19.32evil linux amd64", ""},
		{"error\nMihomo Meta v1.19.32 linux amd64", ""},
	} {
		t.Run(tc.banner, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `source "$1"; core_release_version "$2"`, "version", filepath.Join(repositoryRoot(t), "deploy", "lib", "release.sh"), tc.banner)
			out, err := cmd.CombinedOutput()
			if (err != nil) != (tc.want == "") || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("version=%q err=%v want=%q", out, err, tc.want)
			}
		})
	}
}
