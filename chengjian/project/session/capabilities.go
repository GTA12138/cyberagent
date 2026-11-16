package session

import "reflect"

// CapabilityState distinguishes an explicitly described feature from an unknown
// one. A supported DriverSupport describes code in the adapter, never current
// target reachability, permission, or successful command execution.
type CapabilityState string

const (
	CapabilityUnknown     CapabilityState = "unknown"
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
)

// OperationCapability keeps implementation and target observations separate.
// Metadata alone cannot promote EnvironmentSupport to supported.
type OperationCapability struct {
	DriverSupport      CapabilityState `json:"driver_support"`
	EnvironmentSupport CapabilityState `json:"environment_support"`
}

type OperationCapabilities struct {
	Exec      OperationCapability `json:"exec"`
	ReadFile  OperationCapability `json:"read_file"`
	WriteFile OperationCapability `json:"write_file"`
}

// RuntimeCapabilities contains observed target facts only. Language, Kind, and
// the legacy HTTPShell platform default are not evidence of OS/architecture.
type RuntimeCapabilities struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type OutputStreamMode string

const (
	OutputStreamsUnknown  OutputStreamMode = "unknown"
	OutputStreamsMerged   OutputStreamMode = "merged"
	OutputStreamsSeparate OutputStreamMode = "separate"
)

type CancellationMode string

const (
	CancellationUnknown       CancellationMode = "unknown"
	CancellationLocalWaitOnly CancellationMode = "local_wait_only"
)

// ExecutionCapabilities describes result and cancellation guarantees. In
// particular, nil Exec error does not establish an exit code of zero; local
// cancellation does not establish that a remote process stopped. Persistent job
// tracking refers to an adapter receipt/status API, not shell background syntax.
type ExecutionCapabilities struct {
	OutputStreams         OutputStreamMode `json:"output_streams"`
	ExitCode              CapabilityState  `json:"exit_code"`
	Cancellation          CancellationMode `json:"cancellation"`
	RemoteStopOnCancel    CapabilityState  `json:"remote_stop_on_cancel"`
	PersistentJobTracking CapabilityState  `json:"persistent_job_tracking"`
}

// FileCapabilities deliberately makes no universal binary integrity claim for
// shell-based file operations, whose actual behavior depends on the path used.
type FileCapabilities struct {
	BinaryRead  CapabilityState `json:"binary_read"`
	BinaryWrite CapabilityState `json:"binary_write"`
}

// Capabilities is a versioned, credential-free metadata snapshot. It grants no
// resource access and is not an execution or environment-verification receipt.
type Capabilities struct {
	SchemaVersion int                   `json:"schema_version"`
	Driver        string                `json:"driver"`
	Operations    OperationCapabilities `json:"operations"`
	Runtime       RuntimeCapabilities   `json:"runtime"`
	Execution     ExecutionCapabilities `json:"execution"`
	Files         FileCapabilities      `json:"files"`
}

// CapabilityProvider is optional to preserve the existing Session interface.
// Implementations must read metadata only: no Test, Exec, network requests,
// implicit refresh, file operations, or other target-side activity.
type CapabilityProvider interface {
	Capabilities() Capabilities
}

// HostPlatformProvider is optional and reports the target OS as the driver
// already knows it ("linux" | "windows", "" = not determined).
//
// Why a separate interface instead of Capabilities().Runtime.OS: Runtime is
// deliberately limited to *observed* facts, and the HTTP shell's platform comes
// from the payload language (aspx ⇒ cmd.exe), which is a driver fact rather than
// an observation. Keeping them apart stops a consumer from mistaking "we know
// which shell this payload speaks to" for "we have verified the target OS".
//
// Like CapabilityProvider it must not touch the target: metadata only.
type HostPlatformProvider interface {
	HostPlatform() string
}

// UnknownCapabilities is the conservative snapshot for legacy/unavailable
// drivers. Every field remains explicit in JSON instead of implying support
// through an omitted or false field.
func UnknownCapabilities() Capabilities {
	unknownOperation := OperationCapability{CapabilityUnknown, CapabilityUnknown}
	return Capabilities{
		SchemaVersion: 1,
		Driver:        "unknown",
		Operations: OperationCapabilities{
			Exec: unknownOperation, ReadFile: unknownOperation, WriteFile: unknownOperation,
		},
		Runtime: RuntimeCapabilities{OS: "unknown", Architecture: "unknown"},
		Execution: ExecutionCapabilities{
			OutputStreams:         OutputStreamsUnknown,
			ExitCode:              CapabilityUnknown,
			Cancellation:          CancellationUnknown,
			RemoteStopOnCancel:    CapabilityUnknown,
			PersistentJobTracking: CapabilityUnknown,
		},
		Files: FileCapabilities{BinaryRead: CapabilityUnknown, BinaryWrite: CapabilityUnknown},
	}
}

// DescribeCapabilities never infers support from Kind or invokes Session
// operations. Both a nil interface and a typed nil implementation are unknown.
func DescribeCapabilities(s Session) Capabilities {
	if s == nil || nilSession(s) {
		return UnknownCapabilities()
	}
	if p, ok := s.(CapabilityProvider); ok {
		return p.Capabilities()
	}
	return UnknownCapabilities()
}

func nilSession(s Session) bool {
	v := reflect.ValueOf(s)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// ReverseShellDriverCapabilities describes the existing server reverseShell
// adapter. That adapter opts in explicitly through CapabilityProvider; this
// function must not be selected merely because an arbitrary Kind is "reverse".
func ReverseShellDriverCapabilities() Capabilities {
	return shellDriverCapabilities("reverse_shell")
}

func shellDriverCapabilities(driver string) Capabilities {
	c := UnknownCapabilities()
	c.Driver = driver
	c.Operations.Exec.DriverSupport = CapabilitySupported
	c.Operations.ReadFile.DriverSupport = CapabilitySupported
	c.Operations.WriteFile.DriverSupport = CapabilitySupported
	c.Execution.OutputStreams = OutputStreamsMerged
	c.Execution.ExitCode = CapabilityUnsupported
	c.Execution.Cancellation = CancellationLocalWaitOnly
	c.Execution.RemoteStopOnCancel = CapabilityUnsupported
	c.Execution.PersistentJobTracking = CapabilityUnsupported
	return c
}
