package server

import "chengjian.local/chengjian/session"

func (sh *reverseShell) Capabilities() session.Capabilities {
	if sh == nil {
		return session.UnknownCapabilities()
	}
	return session.ReverseShellDriverCapabilities()
}

func (s *Server) sessionCapabilities(id int64) session.Capabilities {
	if s.sessReg == nil {
		return session.UnknownCapabilities()
	}
	sess, ok := s.sessReg.Get(id)
	if !ok {
		return session.UnknownCapabilities()
	}
	return session.DescribeCapabilities(sess)
}

// sessionPlatform 是「这条会话按什么方言说话」的 agent 可见来源。
//
// 为什么单独一个函数、而不是塞进 capabilities：Capabilities().Runtime.OS 是
// **观测**字段（当前没有任何驱动上报，结构上恒为 unknown），而平台是**驱动
// 元数据**（aspx 马必然对 cmd.exe 说话）。把后者塞进前者会让「我们知道它是
// Windows」和「我们验证过它是 Windows」混成一个值，而这两件事对 agent 的
// 行动后果完全不同。
//
// 读注册表而不是 secret：与 capabilities 同一条路径，不解密任何密文。
// 会话不在注册表（已关闭/恢复失败）时返回 ""，调用方必须当作未知。
func (s *Server) sessionPlatform(id int64) string {
	if s.sessReg == nil {
		return ""
	}
	sess, ok := s.sessReg.Get(id)
	if !ok {
		return ""
	}
	return session.DescribePlatform(sess)
}
