package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv 在开发期读取 .env（若存在），把其中的键值注入进程环境。
//
// 规则：已存在的环境变量优先（不覆盖），因此 .env 只作为本地默认值；
// 文件缺失不算错误。返回实际加载的键数量，便于日志。
func LoadDotEnv(paths ...string) int {
	if len(paths) == 0 {
		paths = []string{".env"}
	}
	count := 0
	for _, p := range paths {
		f, err := os.Open(filepath.Clean(p))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if key == "" {
				continue
			}
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
			if err := os.Setenv(key, value); err != nil {
				continue
			}
			count++
		}
		_ = f.Close()
	}
	return count
}
