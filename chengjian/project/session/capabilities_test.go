package session

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every legacy method panics so the fallback cannot accidentally probe, infer
// support from Kind, or execute merely to describe a session.
type legacyCapabilitySession struct{}

func (legacyCapabilitySession) ID() int64                  { panic("ID called") }
func (legacyCapabilitySession) Kind() string               { panic("Kind called") }
func (legacyCapabilitySession) Test(context.Context) error { panic("Test called") }
func (legacyCapabilitySession) ReadFile(context.Context, string) ([]byte, error) {
	panic("ReadFile called")
}
func (legacyCapabilitySession) WriteFile(context.Context, string, []byte) error {
	panic("WriteFile called")
}
func (legacyCapabilitySession) Close(context.Context) error { panic("Close called") }
func (legacyCapabilitySession) Exec(context.Context, string, time.Duration) (string, string, error) {
	panic("Exec called")
}

type nilCapabilitySession struct{ legacyCapabilitySession }

func (*nilCapabilitySession) Capabilities() Capabilities { panic("typed nil provider called") }

func TestDescribeCapabilitiesLegacyAndNilAreUnknown(t *testing.T) {
	var typedNil *nilCapabilitySession
	var httpNil *HTTPShell
	for name, s := range map[string]Session{
		"nil": nil, "legacy": legacyCapabilitySession{}, "typed_nil": typedNil, "http_typed_nil": httpNil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := DescribeCapabilities(s); !reflect.DeepEqual(got, UnknownCapabilities()) {
				t.Fatalf("got %+v; want conservative unknown", got)
			}
		})
	}
	if got := httpNil.Capabilities(); !reflect.DeepEqual(got, UnknownCapabilities()) {
		t.Fatalf("nil HTTP receiver must remain unknown: %+v", got)
	}
}

type capabilityTransport func(*http.Request) (*http.Response, error)

func (f capabilityTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHTTPMetadataDoesNotProbeOrInferRuntime(t *testing.T) {
	for _, lang := range []string{"php", "phpcmd", "jsp", "aspx"} {
		t.Run(lang, func(t *testing.T) {
			s, err := NewHTTPShell("https://metadata.invalid/private", "credential-secret", lang)
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			s.client.Transport = capabilityTransport(func(*http.Request) (*http.Response, error) {
				requests++
				panic("metadata attempted a network request")
			})
			s.SetPlatform("windows")
			s.SetFuncs([]string{"exec"})
			got := DescribeCapabilities(s)
			if got.Runtime.OS != "unknown" || got.Runtime.Architecture != "unknown" {
				t.Fatalf("language/default/probe cache must not infer runtime: %+v", got.Runtime)
			}
			assertShellCapabilityContract(t, got, "http_shell")
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 0 || strings.Contains(string(data), "credential-secret") || strings.Contains(string(data), "metadata.invalid") {
				t.Fatalf("metadata made requests or exposed connection parameters: %s", data)
			}
		})
	}
}

func TestReverseDriverMetadataIsConservative(t *testing.T) {
	got := ReverseShellDriverCapabilities()
	assertShellCapabilityContract(t, got, "reverse_shell")
	// Snapshots are values. A caller changing its copy cannot promote subsequent
	// snapshots to observed target support.
	got.Operations.Exec.EnvironmentSupport = CapabilitySupported
	if next := ReverseShellDriverCapabilities(); next.Operations.Exec.EnvironmentSupport != CapabilityUnknown {
		t.Fatalf("metadata snapshot was shared: %+v", next)
	}
}

func assertShellCapabilityContract(t *testing.T, got Capabilities, driver string) {
	t.Helper()
	if got.SchemaVersion != 1 || got.Driver != driver {
		t.Fatalf("unexpected driver/schema: %+v", got)
	}
	for name, op := range map[string]OperationCapability{
		"exec": got.Operations.Exec, "read": got.Operations.ReadFile, "write": got.Operations.WriteFile,
	} {
		if op.DriverSupport != CapabilitySupported || op.EnvironmentSupport != CapabilityUnknown {
			t.Errorf("%s must separate implementation from environment: %+v", name, op)
		}
	}
	if got.Execution.OutputStreams != OutputStreamsMerged || got.Execution.ExitCode != CapabilityUnsupported {
		t.Errorf("merged streams must not claim an exit code: %+v", got.Execution)
	}
	if got.Execution.Cancellation != CancellationLocalWaitOnly || got.Execution.RemoteStopOnCancel != CapabilityUnsupported {
		t.Errorf("cancellation must not claim remote termination: %+v", got.Execution)
	}
	if got.Execution.PersistentJobTracking != CapabilityUnsupported || got.Files.BinaryRead != CapabilityUnknown || got.Files.BinaryWrite != CapabilityUnknown {
		t.Errorf("unimplemented job tracking/unverified binary paths overstated: %+v", got)
	}
}
