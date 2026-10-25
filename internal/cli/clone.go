package cli

import (
	"flag"
	"io"
	"strconv"
	"time"
)

// CloneFlags builds independent values with the original defaults. Registering
// defaults must not mark flags as visited, or Parse would ignore file settings.
func CloneFlags(original *flag.FlagSet) *flag.FlagSet {
	fs := flag.NewFlagSet(original.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	original.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" {
			return
		}
		if _, ok := f.Value.(*byteSizeValue); ok {
			v, err := ParseByteSize(f.DefValue)
			if err != nil {
				panic(err)
			}
			ByteSizeVar(fs, new(int), f.Name, v, f.Usage)
			return
		}
		switch f.Value.(flag.Getter).Get().(type) {
		case string:
			fs.String(f.Name, f.DefValue, f.Usage)
		case time.Duration:
			v, e := time.ParseDuration(f.DefValue)
			if e != nil {
				panic(e)
			}
			fs.Duration(f.Name, v, f.Usage)
		case int:
			v, e := strconv.Atoi(f.DefValue)
			if e != nil {
				panic(e)
			}
			fs.Int(f.Name, v, f.Usage)
		case bool:
			v, e := strconv.ParseBool(f.DefValue)
			if e != nil {
				panic(e)
			}
			fs.Bool(f.Name, v, f.Usage)
		default:
			panic("unsupported application flag type")
		}
	})
	return fs
}
