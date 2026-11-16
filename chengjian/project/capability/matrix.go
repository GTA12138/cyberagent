// matrix.go 协议能力矩阵（I2 的剩余四项里最能落地的一项）。
//
// 问题：I2 剩下的四项（协议矩阵 / supports_exit_status 真实获取 / cancel 语义 /
// thin runner）此前被一起挂着，理由是「需要真实 SMB/WinRM/LDAP/SSH 客户端」。
// 这个理由对**实现**成立，对**声明**不成立——矩阵的价值恰恰在于先把「哪些协议
// 真的能打、打到什么程度、能不能拿到退出码、能不能取消」写清楚，让上层在
// 动手之前就知道边界，而不是等执行到一半才发现「这个 method 其实没有退出码」。
//
// 三条判断：
//
//  1. **未实现的协议也进矩阵，标 Implemented=false 并写清原因。** 不列出来的
//     后果是每个调用方各自猜「SMB 应该支持吧」，猜错的代价是一次白跑。
//
//  2. **退出码支持度分三档，不合并成布尔。** real（远端命令真实退出码）/
//     inferred（只有本地客户端进程的退出码）/ none。impacket 家族属于
//     inferred——它返回的是 python 脚本的退出码，不是远端命令的；把它当 real
//     会让「命令失败但脚本成功」被记成成功，这是这类系统最常见的假阳性来源。
//
//  3. **cancel 语义必须写，因为它决定「能不能中途停」。** 能停和不能停对
//     操作节奏影响很大：不能停的动作要一次给足超时，能停的可以长跑。
package capability

import "strings"

// Protocol 是横向/访问协议的标识。
type Protocol string

const (
	ProtoSSH   Protocol = "ssh"
	ProtoSMB   Protocol = "smb"
	ProtoWinRM Protocol = "winrm"
	ProtoLDAP  Protocol = "ldap"
	ProtoRDP   Protocol = "rdp"
)

// ExitStatusSupport 表示「能不能拿到远端命令的真实退出码」。
type ExitStatusSupport string

const (
	// ExitReal 远端回传真实退出码（SSH 的 exec 通道）。
	ExitReal ExitStatusSupport = "real"
	// ExitInferred 只有本地客户端进程的退出码，远端命令的真实结果要靠输出判断。
	ExitInferred ExitStatusSupport = "inferred"
	// ExitNone 完全拿不到。
	ExitNone ExitStatusSupport = "none"
)

// ProtocolSupport 是一种协议的受管能力声明。
type ProtocolSupport struct {
	Protocol Protocol `json:"protocol"`
	// Implemented 表示平台是否真的能执行（false = 只有声明，没有实现）。
	Implemented bool `json:"implemented"`
	// Method 是对应的 lateral_exec method（空 = 还没有入口）。
	Method string `json:"method,omitempty"`
	// Platforms 是目标平台（windows/linux）。
	Platforms []string `json:"platforms,omitempty"`
	// AuthTypes 是支持的凭据类型（db.Cred* 的值）。
	AuthTypes []string `json:"auth_types,omitempty"`
	// ExitStatus 说明退出码可信度——见文件头第 2 条。
	ExitStatus ExitStatusSupport `json:"exit_status"`
	// Cancel 说明「怎么中止一次运行中的执行」，空 = 无法中止。
	Cancel string `json:"cancel,omitempty"`
	// Note 是给 agent/人看的一句话边界说明。
	Note string `json:"note,omitempty"`
	// Missing 在 Implemented=false 时说明缺什么（依赖 / 工作量）。
	Missing string `json:"missing,omitempty"`
}

// matrix 是协议能力矩阵。扩充只加行，不改逻辑。
var matrix = []ProtocolSupport{
	{
		Protocol: ProtoSSH, Implemented: true, Method: "ssh",
		Platforms:  []string{"linux", "windows"},
		AuthTypes:  []string{"password", "ssh_key"},
		ExitStatus: ExitReal,
		Cancel:     "向远端通道发 SIGKILL 并关闭 session；远端进程被真实终止",
		Note: "经 x/crypto/ssh 直连；目标只经隧道可达时自动走任务选路（socks）。" +
			"私钥必须无口令（平台不做交互式解密）。",
	},
	{
		Protocol: ProtoSMB, Implemented: true, Method: "wmiexec",
		Platforms:  []string{"windows"},
		AuthTypes:  []string{"password", "hash_nt", "hash_lm"},
		ExitStatus: ExitInferred,
		Cancel:     "只能杀本地 impacket 进程；**远端已落地的服务/进程可能残留**",
		Note: "impacket 家族（psexec/wmiexec/smbexec/atexec/dcomexec）共用 SMB/RPC 栈。" +
			"退出码是 python 脚本的，不是远端命令的：判定成功要看输出，不能只看 exit=0。",
	},
	{
		Protocol: ProtoWinRM, Implemented: false,
		Platforms: []string{"windows"}, AuthTypes: []string{"password", "hash_nt"},
		ExitStatus: ExitNone,
		Missing:    "需要 WinRM 客户端（pywinrm 或 Go 实现）。当前军火库未钉版，且 WinRM 依赖 5985/5986 端口开放——" +
			"多数内网只开 445，优先用 wmiexec。",
		Note: "未实现：不要假定 lateral_exec 支持 winrm，会直接报不支持。",
	},
	{
		Protocol: ProtoLDAP, Implemented: false,
		Platforms: []string{"windows"}, AuthTypes: []string{"password", "hash_nt"},
		ExitStatus: ExitNone,
		Missing:    "需要 LDAP 客户端（ldapsearch 或 Go ldap 库）+ 目录查询语义。LDAP 不是执行通道，" +
			"它的价值是枚举（用户/组/委派），属于侦察而非横向，应作为独立只读能力引入。",
		Note: "未实现：LDAP 不能执行命令，把它塞进 lateral_exec 会误导 agent。",
	},
	{
		Protocol: ProtoRDP, Implemented: false,
		Platforms: []string{"windows"}, AuthTypes: []string{"password"},
		ExitStatus: ExitNone,
		Missing:    "需要 RDP 客户端 + 图形会话接管；无头环境价值有限，且动静极大（登录即留 4624）。",
		Note: "刻意不实现：RDP 是人工接管场景，不适合自动化横向。",
	},
}

// Matrix 返回协议能力矩阵的副本（调用方可安全修改）。
func Matrix() []ProtocolSupport {
	out := make([]ProtocolSupport, len(matrix))
	copy(out, matrix)
	return out
}

// Lookup 按协议名查能力声明。
func Lookup(p Protocol) (ProtocolSupport, bool) {
	p = Protocol(strings.ToLower(strings.TrimSpace(string(p))))
	for _, m := range matrix {
		if m.Protocol == p {
			return m, true
		}
	}
	return ProtocolSupport{}, false
}

// LookupMethod 按 lateral_exec 的 method 反查协议能力。
//
// 用途是执行前预检：调用方拿到 method 就能知道「这次能不能拿到真实退出码」，
// 从而决定成功判定怎么写，而不是执行完再猜。
func LookupMethod(method string) (ProtocolSupport, bool) {
	method = strings.ToLower(strings.TrimSpace(method))
	for _, m := range matrix {
		if m.Implemented && m.Method == method {
			return m, true
		}
		// impacket 家族共用 SMB 那一行。
		if m.Protocol == ProtoSMB && m.Implemented && isImpacketMethod(method) {
			return m, true
		}
	}
	return ProtocolSupport{}, false
}

// isImpacketMethod 判断 method 是否属于 impacket 家族。
func isImpacketMethod(method string) bool {
	switch method {
	case "psexec", "wmiexec", "smbexec", "atexec", "dcomexec":
		return true
	}
	return false
}

// ExitStatusOf 返回某 method 的退出码可信度；未知 method 返回 ExitNone。
//
// 未知返回 ExitNone 而不是 ExitReal：把「不知道」当成「可信」会让成功判定
// 直接错，而当成「不可信」最多多读一次输出。
func ExitStatusOf(method string) ExitStatusSupport {
	if m, ok := LookupMethod(method); ok {
		return m.ExitStatus
	}
	return ExitNone
}
