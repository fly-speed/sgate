package vpn

import "runtime"

// ValidationError identifies fields without including credential values.
type ValidationError struct {
	Fields  []string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }
func invalidFields(message string, fields ...string) error {
	return &ValidationError{Fields: fields, Message: message}
}

type Support struct {
	Supported bool   `json:"supported"`
	OS        string `json:"os"`
	Reason    string `json:"reason,omitempty"`
}

func PlatformSupport(role string) Support {
	s := Support{Supported: true, OS: runtime.GOOS}
	if role == "server" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		s.Supported = false
		s.Reason = "独立 VPN 服务端仅支持 macOS 和 Linux，当前系统无法启用。"
	}
	if role == "client" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		s.Supported = false
		s.Reason = "独立 VPN 客户端仅支持 macOS、Linux 和 Windows，当前系统无法启用。"
	}
	return s
}
