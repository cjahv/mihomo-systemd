package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const deploymentValidationFile = "validation.json"

type deploymentValidation struct {
	BaselineHash  string `json:"baseline_hash"`
	CandidateHash string `json:"candidate_hash"`
	EnvHash       string `json:"env_hash"`
	InputsHash    string `json:"inputs_hash"`
}

func (u *configUpdater) readDeploymentValidation(candidatePath string) {
	pending, err := os.ReadFile(u.path("deployment.pending"))
	if err != nil {
		return
	}
	stage := strings.TrimSuffix(string(pending), "\n")
	if filepath.Dir(stage) != filepath.Clean(u.dir) || !strings.HasPrefix(filepath.Base(stage), ".deploy.") {
		return
	}
	preparedDir := filepath.Join(stage, "prepared")
	if candidatePath != filepath.Join(preparedDir, "candidate.json") {
		return
	}
	var validation deploymentValidation
	if readJSON(filepath.Join(preparedDir, deploymentValidationFile), &validation) == nil {
		u.preflight = validation
	}
}

func (r *linuxRuntime) validateForDeployment(ctx context.Context, path string) (bool, error) {
	cfg, err := r.validateConfig(ctx, path)
	return err == nil && !hasExternalValidationInputs(cfg), err
}

// Mihomo can open configured keys, certificates and ZeroTier data during
// parsing. Keep the normal validation for these configs.
func hasExternalValidationInputs(value interface{}) bool {
	switch value := value.(type) {
	case map[string]interface{}:
		for key, child := range value {
			switch key {
			case "certificate", "private-key", "client-auth-cert", "planet":
				if path, ok := child.(string); ok && path != "" {
					return true
				}
			}
			if hasExternalValidationInputs(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range value {
			if hasExternalValidationInputs(child) {
				return true
			}
		}
	}
	return false
}

// Native validation resolves GeoData in the live home directory in both
// phases. Hash these inputs, excluding mutable runtime state such as cache.db.
// An empty fingerprint requires the usual validation.
func (r *linuxRuntime) validationInputsHash() string {
	if os.Getenv("CLASH_CONFIG_STRING") != "" {
		return ""
	}
	yq, err := exec.LookPath("yq")
	if err != nil {
		return ""
	}
	paths := []string{r.kernelPath(), yq}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		switch strings.ToLower(entry.Name()) {
		case "geosite.dat", "geoip.dat", "country.mmdb", "geoip.db", "geoip.metadb", "asn.mmdb", "bundlemrs.7z":
			paths = append(paths, filepath.Join(r.dir, entry.Name()))
		}
	}

	inputs := []string{r.dir}
	for i, path := range paths {
		hash := validationFileHash(path)
		if hash == "" {
			return ""
		}
		// The staged core and installed core have different paths.
		if i > 1 {
			inputs = append(inputs, filepath.Base(path))
		}
		inputs = append(inputs, hash)
	}
	var env []string
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "CLASH_") || strings.HasPrefix(value, "SAFE_PATHS=") || strings.HasPrefix(value, "SKIP_SAFE_PATH_CHECK=") {
			env = append(env, value)
		}
	}
	slices.Sort(env)
	encoded, _ := json.Marshal(append(inputs, env...))
	return digest(encoded)
}

func validationFileHash(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
