package cli

import (
	"errors"
	"flag"
	"strconv"
	"strings"
)

// ByteSizeVar 保留整数 flag 的读取方式，并允许配置与命令行使用 K/M 后缀。
func ByteSizeVar(fs *flag.FlagSet, target *int, name string, value int, usage string) {
	*target = value
	fs.Var(&byteSizeValue{target}, name, usage)
}

type byteSizeValue struct{ value *int }

func (v *byteSizeValue) String() string {
	if v == nil || v.value == nil {
		return "0"
	}
	return strconv.Itoa(*v.value)
}
func (v *byteSizeValue) Get() any { return *v.value }
func (v *byteSizeValue) Set(raw string) error {
	value, err := ParseByteSize(raw)
	if err == nil {
		*v.value = value
	}
	return err
}

// ParseByteSize 使用二进制单位：1K = 1024 字节，1M = 1048576 字节。
func ParseByteSize(raw string) (int, error) {
	text := strings.ToUpper(strings.TrimSpace(raw))
	multiplier := uint64(1)
	for _, suffix := range []string{"KIB", "MIB", "KB", "MB", "K", "M", "B"} {
		if strings.HasSuffix(text, suffix) {
			if suffix[0] == 'K' {
				multiplier = 1024
			}
			if suffix[0] == 'M' {
				multiplier = 1024 * 1024
			}
			text = strings.TrimSuffix(text, suffix)
			break
		}
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil || n > uint64(^uint(0)>>1)/multiplier {
		return 0, errors.New("size must be a nonnegative integer with optional K/M suffix")
	}
	return int(n * multiplier), nil
}
