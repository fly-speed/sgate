package app

import (
	"errors"
	"strings"

	"sgate/internal/cli"
)

// master_args 只在整个进程重启后生效；保存前先按实际框架分词规则校验。
// 保留已有 CLI 上下文，避免隐藏的密码等覆盖项在校验时丢失。
func (c *controller) validateMasterArgs(path, raw string) error {
	check := cli.CloneFlags(c.original)
	args := append([]string(nil), c.args...)
	args = append(args, strings.Fields(raw)...)
	args = append(args, "-config", path)
	if err := cli.Parse(check, args); err == nil {
		if _, err = build(c.role, check, nil); err == nil {
			return nil
		}
	}
	// 不返回 flag.Parse 的原错误，它可能把密码或密钥参数的取值写入错误文本。
	return errors.New("master_args 参数无效；请检查选项名称、单位、范围、布尔参数格式和关联约束；按空白拆分，不解析 shell 引号")
}
