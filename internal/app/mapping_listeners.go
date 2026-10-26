package app

import (
	"errors"
	"slices"
	"strings"

	"sgate/internal/cli"
)

// proxyListeners 使用生效参数生成框架监听，保留命令行优先级。
// master_args 是下一次托管启动的命令行，不能用旧进程的覆盖值掩盖其修改。
func (c *controller) proxyListeners(path string) ([]string, error) {
	fs := cli.CloneFlags(c.original)
	args := append([]string(nil), c.args...)
	if c.isManaged() {
		settings, err := processSettings(path)
		if err != nil {
			return nil, err
		}
		old := strings.Fields(c.controls["master_args"])
		for i := 0; len(old) > 0 && i+len(old) <= len(args); i++ {
			if slices.Equal(args[i:i+len(old)], old) {
				args = append(args[:i], args[i+len(old):]...)
				break
			}
		}
		args = append(args, strings.Fields(settings["master_args"])...)
	}
	args = append(args, "-config", path)
	if err := cli.Parse(fs, args); err != nil {
		// flag 的转换错误可能带出参数原值，不把凭据误当成诊断信息返回。
		return nil, errors.New("无法解析生效监听参数，请检查命令行覆盖与 master_args")
	}
	d, err := build(c.role, fs, nil)
	if err != nil {
		return nil, err
	}
	return strings.Split(d.listen, ","), nil
}
