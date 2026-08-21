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

// WorkerOption is exported, so a hand-written option can enable versioning
// while bypassing the validated constructors. The SDK accepts an empty Version
// at both worker.New and RegisterWorkflow, so nothing downstream would catch it.
// versionedOption builds a versioned option the only way the package offers one:
// through the environment, the way a manifest or the Worker Controller supplies
// it.
func versionedOption(t *testing.T, deploymentName, buildID string) WorkerOption {
	t.Helper()
	t.Setenv(EnvDeploymentName, deploymentName)
	t.Setenv(EnvBuildID, buildID)
	opt, err := VersioningFromEnv()
	if err != nil {
		t.Fatalf("VersioningFromEnv(%q, %q): %v", deploymentName, buildID, err)
	}
	return opt
}

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

// A behavior without versioning makes worker.New panic
// (internal_worker.go:2218-2223); normalization drops it instead.
func TestNormalize_DropsBehaviorWhenVersioningOff(t *testing.T) {
	o := resolve(func(o *worker.Options) {
		o.DeploymentOptions.DefaultVersioningBehavior = workflow.VersioningBehaviorAutoUpgrade
	})

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
		wantName       string // expected resolved DeploymentName, when versioning is on
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
			wantName:       "order-worker",
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
		{
			// Exactly what the Worker Controller injects: it composes the
			// server-side name as "<k8s-namespace>/<resource-name>", so a slash
			// must not be treated like the dot the SDK reserves.
			name:           "controller-shaped name with a slash is allowed",
			deploymentName: "order/order-fulfillment",
			buildID:        "order-service-2-4-0-abc123",
			wantName:       "order/order-fulfillment",
			wantVersioning: true,
		},
		{
			// Recovers the trimming coverage the removed MustVersioning tests
			// carried: a manifest with stray spaces must not mint a distinct
			// identity from the same values written cleanly.
			name:           "surrounding whitespace is trimmed",
			deploymentName: "  order-worker  ",
			buildID:        "  v1.5.0  ",
			wantName:       "order-worker",
			wantVersioning: true,
		},
		{
			// The build id is an image tag here, so dots are normal — only the
			// deployment name reserves them.
			name:           "dotted build id is allowed",
			deploymentName: "order-worker",
			buildID:        "v1.5.0-rc.2",
			wantName:       "order-worker",
			wantVersioning: true,
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
			if want := strings.TrimSpace(tt.buildID); tt.wantVersioning && o.DeploymentOptions.Version.BuildID != want {
				t.Errorf("BuildID = %q, want %q", o.DeploymentOptions.Version.BuildID, want)
			}
			if tt.wantVersioning && o.DeploymentOptions.Version.DeploymentName != tt.wantName {
				t.Errorf("DeploymentName = %q, want %q", o.DeploymentOptions.Version.DeploymentName, tt.wantName)
			}
			// Recovers the WithDefaultVersioningBehavior coverage: with the option
			// gone, Pinned must still be what an unset behavior resolves to —
			// upstream's reference worker hard-codes the same value.
			if tt.wantVersioning && o.DeploymentOptions.DefaultVersioningBehavior != workflow.VersioningBehaviorPinned {
				t.Errorf("DefaultVersioningBehavior = %v, want Pinned", o.DeploymentOptions.DefaultVersioningBehavior)
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
	w := NewWorker(c, "order-fulfillment", versionedOption(t, "order-worker", "v1.5.0"))
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
		func(o *worker.Options) {
			o.DeploymentOptions.DefaultVersioningBehavior = workflow.VersioningBehaviorAutoUpgrade
		})
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
		versionedOption(t, "order-worker", "v1.5.0"),
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
