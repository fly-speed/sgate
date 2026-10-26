package admin

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

// syncMappingListeners 只更新已有的框架监听项；旧式独立配置没有此项。
// 与 proxies 共用校验、备份、提交和回滚，不能留下只更新了一半的文件。
func (h *Handler) syncMappingListeners(
	path, content string,
	edits map[string]string,
	mappings []proxyMapping,
) (string, map[string]string, error) {
	fields, err := configFields(content)
	if err != nil {
		return "", nil, err
	}
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		values[field.Key] = field.Value
	}
	current, exists := values["master_service"]
	if !exists {
		return content, nil, nil
	}
	addresses := []string{values["listen"]}
	if len(mappings) > 0 {
		addresses = nil
		for _, mapping := range mappings {
			addresses = append(addresses, mapping.Listen)
		}
	}
	if h.view.ProxyListeners != nil {
		addresses, err = h.view.ProxyListeners(path)
		if err != nil {
			return "", nil, err
		}
	}
	listeners := make([]string, 0, len(addresses))
	for _, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		number, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || number < 1 || number > 65535 {
			return "", nil, errors.New("自动同步 master_service 需要有效的非零 TCP 监听端口")
		}
		endpoint := net.JoinHostPort(host, strconv.Itoa(number))
		last := strings.LastIndexByte(endpoint, ':')
		listeners = append(listeners, endpoint[:last]+"|"+endpoint[last+1:])
	}
	next := strings.Join(listeners, ", ")
	if explicit, ok := edits["master_service"]; ok && !sameMappingListeners(explicit, next) {
		return "", nil, errors.New("master_service 与映射监听地址冲突；请修改映射中的 listen，框架监听会自动同步")
	}
	if sameMappingListeners(current, next) {
		return content, nil, nil
	}
	synced := map[string]string{"master_service": next}
	content, err = updateConfigFields([]byte(content), synced)
	return content, synced, err
}

func sameMappingListeners(a, b string) bool {
	normalize := func(raw string) map[string]bool {
		result := map[string]bool{}
		for _, endpoint := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		}) {
			result[strings.ReplaceAll(endpoint, "|", ":")] = true
		}
		return result
	}
	x, y := normalize(a), normalize(b)
	if len(x) != len(y) {
		return false
	}
	for endpoint := range x {
		if !y[endpoint] {
			return false
		}
	}
	return true
}
