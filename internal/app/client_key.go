package app

import (
	"errors"
	"path/filepath"

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
	if key == nil || key.Value.String() == "" {
		return "", errors.New("请先配置并保存 key-file 路径")
	}
	return filepath.Abs(key.Value.String())
}
