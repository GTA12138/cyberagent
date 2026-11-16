package session

// Capabilities describes the HTTP adapter's implemented paths, not successful
// target operations. It intentionally does not read mutable probe state: cached
// PHP functions, language, and platform defaults cannot establish current
// environment support or trustworthy runtime facts.
func (s *HTTPShell) Capabilities() Capabilities {
	if s == nil {
		return UnknownCapabilities()
	}
	return shellDriverCapabilities("http_shell")
}

var _ CapabilityProvider = (*HTTPShell)(nil)

// HostPlatform reports the shell language's native command interpreter target:
// aspx payloads run under cmd.exe on Windows, php/jsp payloads under a Unix
// shell. It is a driver fact derived from the payload language, not an
// observation of the target — see HostPlatformProvider.
//
// 消费者（tunnel 的平台适配）需要它来选目标侧命令语法：此前只能按 Kind
// 字符串猜，一旦换了命名（http_aspx_enc 之类）就会猜错，把 cmd 命令发给 sh。
func (s *HTTPShell) HostPlatform() string {
	if s == nil {
		return ""
	}
	return s.platform
}

var _ HostPlatformProvider = (*HTTPShell)(nil)
