package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The kernel uses cache mtime as updatedAt. Preserve it across staging and recovery.
type providerCache struct {
	data     []byte
	modified time.Time
}

func readProviderCache(root, name string) (providerCache, error) {
	path, err := providerPath(root, name)
	if err != nil {
		return providerCache{}, err
	}
	file, err := os.Open(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) {
		return providerCache{}, nil
	}
	if err != nil {
		return providerCache{}, err
	}
	defer file.Close()
	data, err := readLimitedProvider(file)
	if err != nil {
		return providerCache{}, err
	}
	stat, err := file.Stat()
	if err != nil {
		return providerCache{}, err
	}
	return providerCache{data, stat.ModTime()}, nil
}

// interval=0 (including omitted) disables periodic refresh, as in Mihomo.
func providerInterval(options map[string]interface{}) (time.Duration, error) {
	value := options["interval"]
	if value == nil {
		return 0, nil
	}
	var seconds float64
	var err error
	switch v := value.(type) {
	case float64:
		seconds = v
	case int:
		seconds = float64(v)
	case json.Number:
		seconds, err = v.Float64()
	case string:
		seconds, err = strconv.ParseFloat(v, 64)
	default:
		err = errors.New("interval 必须为非负整数秒")
	}
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds != math.Trunc(seconds) || seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, errors.New("interval 必须为可表示的非负整数秒")
	}
	return time.Duration(seconds) * time.Second, nil
}

func (c providerCache) fresh(now time.Time, interval time.Duration) bool {
	if len(c.data) == 0 || c.modified.IsZero() || c.modified.After(now) {
		return false
	}
	return interval == 0 || now.Sub(c.modified) < interval
}

func providerResourceTimes(root string, resources map[string][]byte) (map[string]time.Time, error) {
	if len(resources) == 0 {
		return nil, nil
	}
	times := make(map[string]time.Time, len(resources))
	for name, data := range resources {
		cache, err := readProviderCache(root, name)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(cache.data, data) {
			return nil, fmt.Errorf("provider %s 在建立快照期间发生变化", name)
		}
		times[name] = cache.modified
	}
	return times, nil
}

func writeProviderSnapshot(root string, resources map[string][]byte, times map[string]time.Time) error {
	total := 0
	for name, data := range resources {
		total += len(data)
		if len(data) == 0 || len(data) > maxConfigSize || total > maxProviderBytes {
			return errors.New("provider 快照为空或超过资源大小限制")
		}
		path, err := providerPath(root, name)
		if err != nil {
			return err
		}
		path = filepath.Join(root, path)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		modified := times[name]
		if modified.IsZero() {
			// A recovery record without timestamps cannot establish fresh HTTP data.
			modified = time.Unix(0, 0)
		}
		if err = atomicWriteAt(path, data, 0600, modified); err != nil {
			return err
		}
	}
	return nil
}

// A successful refresh can return identical bytes. Advance only the timestamp,
// without replacing a live cache or moving a newer kernel refresh backwards.
func refreshProviderTimes(root string, resources map[string][]byte, times map[string]time.Time) error {
	for name, data := range resources {
		cache, err := readProviderCache(root, name)
		if err != nil {
			return err
		}
		if !bytes.Equal(cache.data, data) {
			return fmt.Errorf("provider %s 在准备期间发生变化", name)
		}
		modified := times[name]
		if !modified.After(cache.modified) {
			continue
		}
		path := filepath.Join(root, name)
		if err = os.Chtimes(path, modified, modified); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
