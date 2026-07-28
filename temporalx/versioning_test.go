package temporalx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// resolve runs options through the same composition + normalization NewWorker
// uses, without needing a client.
func resolve(opts ...WorkerOption) worker.Options {
	var o worker.Options
	for _, opt := range opts {
		opt(&o)
	}
	normalizeVersioning(&o)
	return o
}

func TestMustVersioning_SetsDeploymentOptions(t *testing.T) {
	o := resolve(MustVersioning("order-worker", "v1.5.0"))

	if !o.DeploymentOptions.UseVersioning {
		t.Error("UseVersioning = false, want true")
	}
	if got := o.DeploymentOptions.Version.DeploymentName; got != "order-worker" {
		t.Errorf("DeploymentName = %q, want %q", got, "order-worker")
	}
	if got := o.DeploymentOptions.Version.BuildID; got != "v1.5.0" {
		t.Errorf("BuildID = %q, want %q", got, "v1.5.0")
	}
	if got := o.DeploymentOptions.DefaultVersioningBehavior; got != workflow.VersioningBehaviorPinned {
		t.Errorf("DefaultVersioningBehavior = %v, want Pinned", got)
	}
}

func TestMustVersioning_TrimsWhitespace(t *testing.T) {
	o := resolve(MustVersioning("  order-worker  ", "  v1.5.0  "))

	if got := o.DeploymentOptions.Version; got.DeploymentName != "order-worker" || got.BuildID != "v1.5.0" {
		t.Errorf("Version = %+v, want {order-worker v1.5.0}", got)
	}
}

// An invalid identifier is a program constant being wrong, and the SDK would
// accept it silently — so it must not build a worker.
func TestMustVersioning_PanicsOnInvalidIdentifiers(t *testing.T) {
	tests := []struct {
		name           string
		deploymentName string
		buildID        string
		wantMsg        string
	}{
		{"empty deployment name", "", "v1", "deployment name is empty"},
		{"whitespace-only deployment name", "   ", "v1", "deployment name is empty"},
		{"empty build id", "order-worker", "", "build id is empty"},
		{"whitespace-only build id", "order-worker", "\t", "build id is empty"},
		{"dotted deployment name", "order.saga", "v1", "reserves to join"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("MustVersioning(%q, %q) did not panic", tt.deploymentName, tt.buildID)
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, tt.wantMsg) {
					t.Errorf("panic = %v, want it to mention %q", r, tt.wantMsg)
				}
			}()
			MustVersioning(tt.deploymentName, tt.buildID)
		})
	}
}

// Dots are legitimate in a build id — only the deployment name is constrained,
// because the SDK splits the canonical string on the FIRST dot.
func TestMustVersioning_AllowsDottedBuildID(t *testing.T) {
	o := resolve(MustVersioning("order-worker", "v1.5.0-rc.2"))

	if got := o.DeploymentOptions.Version.BuildID; got != "v1.5.0-rc.2" {
		t.Errorf("BuildID = %q, want %q", got, "v1.5.0-rc.2")
	}
}

// Option order must not change the worker: "behavior then identity" is as
// natural to write as the reverse, and the wrong one silently pinning every
// workflow would keep old deployment versions from ever draining.
func TestVersioningOptions_AreOrderInsensitive(t *testing.T) {
	behaviorFirst := resolve(
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorAutoUpgrade),
		MustVersioning("order-worker", "v1.5.0"),
	)
	versioningFirst := resolve(
		MustVersioning("order-worker", "v1.5.0"),
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorAutoUpgrade),
	)

	if behaviorFirst.DeploymentOptions != versioningFirst.DeploymentOptions {
		t.Fatalf("option order changed the result:\n behavior-first  = %+v\n versioning-first = %+v",
			behaviorFirst.DeploymentOptions, versioningFirst.DeploymentOptions)
	}
	if got := behaviorFirst.DeploymentOptions.DefaultVersioningBehavior; got != workflow.VersioningBehaviorAutoUpgrade {
		t.Errorf("DefaultVersioningBehavior = %v, want AutoUpgrade in both orders", got)
	}
}

// Unspecified cannot remove the default: with versioning on, a workflow that
// ends up with no behavior panics at registration.
func TestWithDefaultVersioningBehavior_UnspecifiedKeepsPinnedDefault(t *testing.T) {
	o := resolve(
		MustVersioning("order-worker", "v1.5.0"),
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorUnspecified),
	)

	if got := o.DeploymentOptions.DefaultVersioningBehavior; got != workflow.VersioningBehaviorPinned {
		t.Errorf("DefaultVersioningBehavior = %v, want Pinned", got)
	}
}

// Unspecified must not overwrite an explicit choice either. A service that
// assembles options conditionally can easily append a behavior read from a
// zero-valued config field after an explicit one; downgrading AutoUpgrade to
// Pinned there would strand the old deployment version, and the startup log
// would read "pinned" with no hint an override was discarded.
func TestWithDefaultVersioningBehavior_UnspecifiedDoesNotOverwrite(t *testing.T) {
	o := resolve(
		MustVersioning("order-worker", "v1.5.0"),
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorAutoUpgrade),
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorUnspecified),
	)

	if got := o.DeploymentOptions.DefaultVersioningBehavior; got != workflow.VersioningBehaviorAutoUpgrade {
		t.Errorf("DefaultVersioningBehavior = %v, want AutoUpgrade preserved", got)
	}
}

// An out-of-range behavior passes worker.New and RegisterWorkflow, then panics
// inside the workflow task handler on the first behavior-less workflow task. It
// has to fail at construction like every other bad input in this package.
func TestWithDefaultVersioningBehavior_PanicsOnUnknown(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("WithDefaultVersioningBehavior(99) did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "unknown behavior 99") {
			t.Errorf("panic = %v, want it to name the unknown behavior", r)
		}
	}()
	WithDefaultVersioningBehavior(workflow.VersioningBehavior(99))
}

// WorkerOption is exported, so a hand-written option can enable versioning
// while bypassing the validated constructors. The SDK accepts an empty Version
// at both worker.New and RegisterWorkflow, so nothing downstream would catch it.
func TestNewWorker_PanicsOnVersioningWithoutIdentity(t *testing.T) {
	c := lazyClient(t)
	defer c.Close()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an empty versioning identity did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "valid identity") {
			t.Errorf("panic = %v, want it to mention a valid identity", r)
		}
	}()
	// Through NewWorker, not resolve(): this also pins that DeploymentOptions
	// survives the trip into worker construction rather than being zeroed there.
	NewWorker(c, "order-fulfillment", func(o *worker.Options) {
		o.DeploymentOptions.UseVersioning = true
	})
}

// The same escape hatch applies to the behavior: a valid identity plus an
// out-of-range behavior passes worker.New and RegisterWorkflow, then panics in
// the SDK's task handler on the first behavior-less workflow task.
func TestNewWorker_PanicsOnUnknownBehaviorFromRawOption(t *testing.T) {
	c := lazyClient(t)
	defer c.Close()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an out-of-range behavior did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "unknown default versioning behavior 99") {
			t.Errorf("panic = %v, want it to name the unknown behavior", r)
		}
	}()
	NewWorker(c, "order-fulfillment", func(o *worker.Options) {
		o.DeploymentOptions.UseVersioning = true
		o.DeploymentOptions.Version = worker.WorkerDeploymentVersion{DeploymentName: "order-worker", BuildID: "v1"}
		o.DeploymentOptions.DefaultVersioningBehavior = workflow.VersioningBehavior(99)
	})
}

// Versioning is the checked constructor: identifiers that arrive at runtime must
// be reportable as an error, not only as a panic.
func TestVersioning_ReturnsErrorOnInvalidIdentifiers(t *testing.T) {
	tests := []struct {
		name           string
		deploymentName string
		buildID        string
		wantErrPart    string
	}{
		{"empty deployment name", "", "v1", "deployment name is empty"},
		{"empty build id", "order-worker", "", "build id is empty"},
		{"dotted deployment name", "order.saga", "v1", "reserves to join"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt, err := Versioning(tt.deploymentName, tt.buildID)
			if err == nil {
				t.Fatalf("Versioning(%q, %q) = nil error, want one", tt.deploymentName, tt.buildID)
			}
			if !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrPart)
			}
			if opt != nil {
				t.Error("option must be nil when the identifiers are rejected")
			}
		})
	}
}

func TestVersioning_ReturnsUsableOption(t *testing.T) {
	opt, err := Versioning("  order-worker  ", "  v1.5.0  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	o := resolve(opt)
	if got := o.DeploymentOptions.Version; got.DeploymentName != "order-worker" || got.BuildID != "v1.5.0" {
		t.Errorf("Version = %+v, want {order-worker v1.5.0}", got)
	}
}

// A behavior without versioning makes worker.New panic
// (internal_worker.go:2218-2223); normalization drops it instead.
func TestNormalize_DropsBehaviorWhenVersioningOff(t *testing.T) {
	o := resolve(WithDefaultVersioningBehavior(workflow.VersioningBehaviorAutoUpgrade))

	if o.DeploymentOptions.UseVersioning {
		t.Error("UseVersioning = true, want false")
	}
	if got := o.DeploymentOptions.DefaultVersioningBehavior; got != workflow.VersioningBehaviorUnspecified {
		t.Errorf("DefaultVersioningBehavior = %v, want Unspecified", got)
	}
}

func TestVersioningFromEnv(t *testing.T) {
	const unset = "\x00" // sentinel: leave the variable absent

	tests := []struct {
		name           string
		deploymentName string
		buildID        string
		wantVersioning bool
		wantErrParts   []string
	}{
		{
			// The normal case for every service that has not opted in.
			name:           "both absent disables versioning",
			deploymentName: unset,
			buildID:        unset,
		},
		{
			name:           "both set enables versioning",
			deploymentName: "order-worker",
			buildID:        "v1.5.0",
			wantVersioning: true,
		},
		{
			// Present-but-empty must NOT read as absent: that is how a worker
			// ends up silently unversioned while everything else says versioned.
			name:           "both present but empty is an error",
			deploymentName: "",
			buildID:        "",
			wantErrParts:   []string{"deployment name is empty", EnvDeploymentName},
		},
		{
			name:           "whitespace only is an error",
			deploymentName: "order-worker",
			buildID:        "   ",
			wantErrParts:   []string{"build id is empty"},
		},
		{
			name:           "build id without deployment name is an error",
			deploymentName: unset,
			buildID:        "v1.5.0",
			wantErrParts:   []string{"deployment name is empty", "v1.5.0"},
		},
		{
			name:           "deployment name without build id is an error",
			deploymentName: "order-worker",
			buildID:        unset,
			wantErrParts:   []string{"build id is empty", "order-worker"},
		},
		{
			name:           "dotted deployment name is an error",
			deploymentName: "order.saga",
			buildID:        "v1.5.0",
			wantErrParts:   []string{"reserves to join"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setOrUnset(t, EnvDeploymentName, tt.deploymentName, unset)
			setOrUnset(t, EnvBuildID, tt.buildID, unset)

			opt, err := VersioningFromEnv()

			if len(tt.wantErrParts) > 0 {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				for _, want := range tt.wantErrParts {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err.Error(), want)
					}
				}
				if opt != nil {
					t.Error("option must be nil when the config is rejected")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			o := resolve(opt)
			if got := o.DeploymentOptions.UseVersioning; got != tt.wantVersioning {
				t.Errorf("UseVersioning = %v, want %v", got, tt.wantVersioning)
			}
			if tt.wantVersioning && o.DeploymentOptions.Version.BuildID != tt.buildID {
				t.Errorf("BuildID = %q, want %q", o.DeploymentOptions.Version.BuildID, tt.buildID)
			}
		})
	}
}

// setOrUnset makes the variable absent for the sentinel and set otherwise, so
// the table can distinguish "absent" from "present but empty" — the whole point
// of reading the environment with LookupEnv.
func setOrUnset(t *testing.T, key, value, sentinel string) {
	t.Helper()
	if value == sentinel {
		t.Setenv(key, "") // registers cleanup, then remove for this test
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		return
	}
	t.Setenv(key, value)
}

func TestMustVersioningFromEnv_AbsentIsNoOp(t *testing.T) {
	const unset = "\x00"
	setOrUnset(t, EnvDeploymentName, unset, unset)
	setOrUnset(t, EnvBuildID, unset, unset)

	o := resolve(MustVersioningFromEnv())

	if o.DeploymentOptions.UseVersioning {
		t.Error("UseVersioning = true with the env absent, want false")
	}
}

func TestMustVersioningFromEnv_ReadsEnv(t *testing.T) {
	t.Setenv(EnvDeploymentName, "order-worker")
	t.Setenv(EnvBuildID, "abc123")

	o := resolve(MustVersioningFromEnv())

	if !o.DeploymentOptions.UseVersioning {
		t.Fatal("UseVersioning = false, want true")
	}
	if got := o.DeploymentOptions.Version; got.DeploymentName != "order-worker" || got.BuildID != "abc123" {
		t.Errorf("Version = %+v, want {order-worker abc123}", got)
	}
}

// The options must actually reach worker.New. Every test above resolves them in
// isolation, so without this one NewWorker could drop the option loop entirely
// and stay green — the exact "looks versioned, polls unversioned" outcome this
// package exists to prevent.
//
// The probe is a value the SDK rejects at worker.New:
// "cannot set MaxConcurrentWorkflowTaskExecutionSize to 1"
// (internal_worker.go:2208-2209). It panics only if the option arrived.
func TestNewWorker_AppliesOptions(t *testing.T) {
	c, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1", Namespace: "mop"})
	if err != nil {
		t.Fatalf("NewLazyClient: %v", err)
	}
	defer c.Close()

	probe := func(o *worker.Options) { o.MaxConcurrentWorkflowTaskExecutionSize = 1 }

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("worker.New did not reject the probe option — NewWorker discarded its options")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "MaxConcurrentWorkflowTaskExecutionSize") {
			t.Errorf("panic = %v, want the SDK's MaxConcurrentWorkflowTaskExecutionSize rejection", r)
		}
	}()
	NewWorker(c, "order-fulfillment", probe)
}

// Conversely, a real versioned worker must be constructible: worker.New panics
// on several DeploymentOptions combinations, so this pins that the normalized
// output is one the SDK accepts.
func TestNewWorker_VersionedWorkerIsAccepted(t *testing.T) {
	c, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1", Namespace: "mop"})
	if err != nil {
		t.Fatalf("NewLazyClient: %v", err)
	}
	defer c.Close()

	// No explicit behavior on purpose: registration is where the SDK enforces
	// that a versioned workflow HAS one (internal_worker.go:1232-1235), so this
	// only survives if NewWorker normalized Unspecified to Pinned. Passing an
	// explicit behavior here would satisfy the guard either way and prove
	// nothing about normalization.
	w := NewWorker(c, "order-fulfillment", MustVersioning("order-worker", "v1.5.0"))
	if w == nil {
		t.Fatal("NewWorker returned nil")
	}
	w.RegisterWorkflow(probeWorkflow)
}

// A behavior-less workflow: registering it proves the default reached the SDK.
func probeWorkflow(ctx workflow.Context) error { return nil }

// The exit on a half-configured worker is the safety property this whole change
// rests on, so it is verified rather than asserted: re-exec this test as a
// subprocess with exactly one variable set and check it dies with a message that
// names the missing one.
func TestMustVersioningFromEnv_ExitsOnPartialConfig(t *testing.T) {
	const probeVar = "TEMPORALX_FATAL_PROBE"

	if os.Getenv(probeVar) == "1" {
		MustVersioningFromEnv() // must not return
		return
	}

	env := []string{probeVar + "=1", EnvDeploymentName + "=order-worker"}
	for _, kv := range os.Environ() {
		// Drop any inherited build id, or the child would be validly configured.
		if !strings.HasPrefix(kv, EnvBuildID+"=") && !strings.HasPrefix(kv, EnvDeploymentName+"=") {
			env = append(env, kv)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("subprocess err = %v (output %q), want a non-zero exit", err, out)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(string(out), EnvBuildID) {
		t.Errorf("output %q does not name the missing %s", out, EnvBuildID)
	}
}

func TestBehaviorName(t *testing.T) {
	tests := []struct {
		behavior workflow.VersioningBehavior
		want     string
	}{
		{workflow.VersioningBehaviorPinned, "pinned"},
		{workflow.VersioningBehaviorAutoUpgrade, "auto-upgrade"},
		// Defensive only: WithDefaultVersioningBehavior rejects out-of-range
		// values, so this is unreachable through the exported API. Pinned so a
		// log line can never render a bare ordinal if that ever changes.
		{workflow.VersioningBehavior(99), "unknown(99)"},
	}

	for _, tt := range tests {
		if got := behaviorName(tt.behavior); got != tt.want {
			t.Errorf("behaviorName(%d) = %q, want %q", int(tt.behavior), got, tt.want)
		}
	}
}

// The off branch of normalization also has to hold at the worker.New boundary:
// a behavior set without versioning makes worker.New panic
// (internal_worker.go:2218-2223), so this only passes if NewWorker dropped it.
func TestNewWorker_BehaviorWithoutVersioningIsAccepted(t *testing.T) {
	c := lazyClient(t)
	defer c.Close()

	w := NewWorker(c, "order-fulfillment",
		WithDefaultVersioningBehavior(workflow.VersioningBehaviorAutoUpgrade))
	if w == nil {
		t.Fatal("NewWorker returned nil")
	}
}

// A nil option means a caller ignored VersioningFromEnv's error. Skipping it
// would build an unversioned worker, so it must fail loudly and say why.
func TestNewWorker_PanicsOnNilOption(t *testing.T) {
	c := lazyClient(t)
	defer c.Close()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a nil WorkerOption did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "VersioningFromEnv error was ignored") {
			t.Errorf("panic = %v, want it to name the ignored error", r)
		}
	}()
	NewWorker(c, "order-fulfillment", nil)
}

func lazyClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:1", Namespace: "mop"})
	if err != nil {
		t.Fatalf("NewLazyClient: %v", err)
	}
	return c
}

// capturingPlugin records the options worker.New actually received.
// ConfigureWorker is handed the live *worker.Options before validation
// (internal_worker.go:2179), which is the only way to assert from outside that
// versioning survived the trip into the SDK.
type capturingPlugin struct {
	worker.PluginBase
	saw worker.DeploymentOptions
}

func (p *capturingPlugin) Name() string { return "temporalx-test-capture" }

func (p *capturingPlugin) ConfigureWorker(_ context.Context, o worker.PluginConfigureWorkerOptions) error {
	p.saw = o.WorkerOptions.DeploymentOptions
	return nil
}

// Every other test observes versioning through this package's own helpers, so
// none of them would notice a NewWorker that normalized a COPY and passed the
// original — normalization's panics would still fire while the SDK quietly got
// an unversioned worker. This asserts the fields the SDK received.
func TestNewWorker_SDKReceivesVersioning(t *testing.T) {
	c := lazyClient(t)
	defer c.Close()

	captured := &capturingPlugin{}
	NewWorker(c, "order-fulfillment",
		MustVersioning("order-worker", "v1.5.0"),
		func(o *worker.Options) { o.Plugins = append(o.Plugins, captured) },
	)

	if !captured.saw.UseVersioning {
		t.Error("worker.New received UseVersioning = false, want true")
	}
	want := worker.WorkerDeploymentVersion{DeploymentName: "order-worker", BuildID: "v1.5.0"}
	if captured.saw.Version != want {
		t.Errorf("worker.New received Version = %+v, want %+v", captured.saw.Version, want)
	}
	if got := captured.saw.DefaultVersioningBehavior; got != workflow.VersioningBehaviorPinned {
		t.Errorf("worker.New received DefaultVersioningBehavior = %v, want Pinned", got)
	}
}
