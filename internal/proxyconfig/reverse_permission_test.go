package proxyconfig

import (
	"encoding/json"
	"testing"
)

func TestMappingReversePermissionDefaultsAndStrictType(t *testing.T) {
	for _, test := range []struct {
		extra string
		want  bool
	}{
		{extra: ""}, {extra: `,"allow_reverse":false`}, {extra: `,"allow_reverse":true`, want: true},
	} {
		mappings, err := Decode([]byte(`[{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel"` + test.extra + `}]`))
		if err != nil || mappings[0].AllowReverse != test.want {
			t.Fatal(test, err)
		}
		raw, err := json.Marshal(mappings)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(raw)
		if err != nil || again[0].AllowReverse != test.want {
			t.Fatal("permission lost on roundtrip", err)
		}
	}
	for _, value := range []string{`"true"`, `1`, `{}`, `[]`} {
		if _, err := Decode([]byte(`[{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel","allow_reverse":` + value + `}]`)); err == nil {
			t.Fatal("nonboolean permission accepted", value)
		}
	}
}
