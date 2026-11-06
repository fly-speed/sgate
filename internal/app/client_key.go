package app

import (
	"errors"
	"path/filepath"
	"slices"

	"sgate/internal/cli"
)

func (c *controller) clientKeyPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return "", errors.New("无法读取客户端密钥配置")
	}
	key := fs.Lookup("key-file")
	if key == nil {
		return "", errors.New("请先配置并保存 key-file 路径")
	}
	path := cli.NormalizeKeyFilePath(key.Value.String())
	if path == "" {
		return "", errors.New("请先配置并保存 key-file 路径")
	}
	return filepath.Abs(path)
}

func (c *controller) serverVPNFilePaths() (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	values, err := cli.ReadValues(c.path)
	if err != nil {
		return nil, err
	}
	fs := cli.CloneFlags(c.original)
	if err := cli.Parse(fs, c.args); err != nil {
		return nil, err
	}
	paths := make(map[string]string, 3)
	for _, base := range []string{"tls-cert", "tls-key", "key-file"} {
		key := "vpn-" + base
		path := values[key]
		if slices.Contains(c.overrides, key) {
			path = fs.Lookup(key).Value.String()
		}
		if base == "key-file" {
			path = cli.NormalizeKeyFilePath(path)
		}
		if path == "" {
			path = fs.Lookup(base).Value.String()
		}
		if base == "key-file" {
			path = cli.NormalizeKeyFilePath(path)
			paths[key] = ""
		}
		if path != "" {
			paths[key], err = filepath.Abs(path)
			if err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}
