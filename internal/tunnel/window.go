package tunnel

import (
	"errors"
	"strconv"
)

const receiveWindowQuery = "sgate-window"
const receiveWindowHeader = "X-Sgate-Receive-Window"

var errReceiveGap = errors.New("gap in received sequence")

func parseReceiveWindow(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < frameHeader+chunkSize {
		return 0, errors.New("invalid receive window")
	}
	return n, nil
}

// 请求窗口包含在已认证的 RequestURI 中；响应窗口另纳入服务端证明。
// 没有窗口头的旧服务端仍按原证明校验，避免无意改变旧版协议的握手。
func windowMAC(key []byte, window string, parts ...string) string {
	if window != "" {
		parts = append(parts, "receive-window", window)
	}
	return mac(key, parts...)
}
