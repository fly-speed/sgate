package admin

import (
	"encoding/json"
	"errors"
	"slices"
)

// ErrMappingServiceRestart requests a process restart after saving managed port changes.
var ErrMappingServiceRestart = errors.New("acl_master mapping listener change requires service restart")

// 待重启标记只比较非热参数，避免在线更新误报“待应用”。
func (h *Handler) restartDigest(content []byte) string {
	if h.view.ApplyLive == nil {
		return digest(content)
	}
	fields, err := configFields(string(content))
	if err != nil {
		return digest(content)
	}
	values := map[string]string{}
	for _, field := range fields {
		if !slices.Contains(h.view.LiveFields, field.Key) {
			values[field.Key] = field.Value
		}
	}
	encoded, _ := json.Marshal(values)
	return digest(encoded)
}

func (h *Handler) changedLiveFields(current []byte, content string) []string {
	if h.view.ApplyLive == nil {
		return nil
	}
	oldFields, e1 := configFields(string(current))
	newFields, e2 := configFields(content)
	if e1 != nil || e2 != nil {
		return nil
	}
	old := map[string]string{}
	for _, field := range oldFields {
		old[field.Key] = field.Value
	}
	next := map[string]string{}
	for _, field := range newFields {
		next[field.Key] = field.Value
	}
	var keys []string
	for _, key := range h.view.LiveFields {
		if old[key] != next[key] {
			keys = append(keys, key)
		}
	}
	return keys
}
