package tunnel

import "testing"

func TestParseNetworkMemoryDenied(t *testing.T) {
	valid := "323/1101 mbufs in use:\n18345 KB allocated to network (1.8% in use)\n"
	for _, tc := range []struct {
		name, text string
		want       int64
	}{
		{"omitted zero", valid, 0},
		{"nonzero", valid + "547957 requests for memory denied\n", 547957},
		{"explicit zero", valid + "0 requests for memory denied\n", 0},
		{"unavailable", "", -1},
		{"unrecognized", "permission denied", -1},
		{"malformed count", valid + "unknown requests for memory denied\n", -1},
		{"negative count", valid + "-1 requests for memory denied\n", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNetworkMemoryDenied(tc.text); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
