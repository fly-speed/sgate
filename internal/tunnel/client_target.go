package tunnel

import (
	"errors"
	"net/url"
	"strconv"
)

// clientEndpoint 先构造最终 URL，再由拨号握手签名，确保 addr 同样受完整性保护。
// 独立 addr 非空时覆盖 URL 中的选择；留空则兼容已有 server?addr= 写法。
// 关闭地址发送时移除所有 addr 参数，其他查询参数继续参与握手签名。
func clientEndpoint(endpoint, address string, omitAddr bool) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "quic") || u.User != nil || u.Fragment != "" {
		return "", errors.New("server URL must be ws://host/path or wss://host/path or quic://host:port/path")
	}
	if u.Scheme == "quic" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", errors.New("QUIC server URL requires an explicit UDP port in 1..65535")
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("invalid server URL query")
	}
	if omitAddr {
		query.Del("addr")
		u.RawQuery = query.Encode()
	} else if address != "" {
		if err := validateSelectableTarget(address); err != nil {
			return "", errors.New("addr must be a valid IP:port or hostname:port with a numeric port")
		}
		query.Set("addr", address)
		u.RawQuery = query.Encode()
	} else if addresses, present := query["addr"]; present {
		if len(addresses) != 1 || validateSelectableTarget(addresses[0]) != nil {
			return "", errors.New("server URL requires exactly one nonempty valid addr")
		}
	}
	return u.String(), nil
}
