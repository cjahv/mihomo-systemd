package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
)

// Rule and proxy providers are downloaded, cached and refreshed by the kernel itself.
// The manager only reads their declarations to confirm the kernel initialized them.
type providerSpec struct {
	kind, name string
}

func parseConfig(ctx context.Context, path string) (map[string]interface{}, error) {
	cmd := exec.CommandContext(ctx, "yq", "-o=json", ".", path)
	b, err := diagnosticCommandOutput(ctx, "yq 解析 YAML（"+path+"）", cmd)
	if err != nil {
		return nil, err
	}
	var cfg map[string]interface{}
	if json.Unmarshal(b, &cfg) != nil || len(cfg) == 0 {
		return nil, errors.New("配置必须是非空 YAML 对象")
	}
	return cfg, nil
}

// Inline providers have no file or network source, so the kernel has nothing to initialize for them.
func providers(cfg map[string]interface{}) ([]providerSpec, error) {
	var result []providerSpec
	for _, kind := range []string{"rule-providers", "proxy-providers"} {
		v := cfg[kind]
		if v == nil {
			continue
		}
		items, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s 必须是对象", kind)
		}
		names := make([]string, 0, len(items))
		for name := range items {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			options, ok := items[name].(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("%s 的条目必须是对象", kind)
			}
			typ, _ := options["type"].(string)
			if typ == "inline" {
				continue
			}
			if typ != "http" && typ != "file" {
				return nil, fmt.Errorf("%s 的 type 必须为 http、file 或 inline", kind)
			}
			result = append(result, providerSpec{kind: kind, name: name})
		}
	}
	return result, nil
}
