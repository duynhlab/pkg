package temporalx

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Worker Deployment Versioning (RFC-0021 P3, homelab ADR-030).
//
// Versioning lets the server pin every workflow to the worker deployment
// version that started it: a new build takes new workflows while in-flight ones
// drain on the old build, so a workflow migration needs no in-code version
// markers. It requires Temporal server >= 1.29.1.
//
// Versioning is OPT-IN per service: a worker built with no option behaves
// exactly as it did before this file existed.
//
// # Turning it on is TWO steps, not one
//
// Setting the env vars only makes this worker POLL as versioned. It does not
// route anything to it. Until an operator sets the deployment's Current
// Version server-side, that version is nil, and the SDK is explicit about what
// nil means: "CurrentVersion - Specifies which Deployment Version should
// receive new workflow executions, and tasks of existing non-pinned workflows.
// If nil, all unversioned workers are the target."
// (go.temporal.io/sdk@v1.44.1/internal/internal_versioning_client.go:248-250)
//
// So if every worker on a task queue is switched to versioned and no Current
// Version is set, new workflows are accepted by the frontend and then dispatched
// to unversioned workers — of which there are none. Nothing crashes, no error is
// logged, pods stay Ready, and the task queue backlogs silently. The second step
//
//	temporal worker-deployment set-current-version \
//	  --deployment-name <name> --build-id <id>
//
// is what actually cuts traffic over. Treat the env flip and that command as one
// atomic operation in the rollout runbook.
//
// # Pinned carries a deployment contract
//
// Workflows default to Pinned here (see WithVersioning), which means a rolling
// update that replaces the old version's pods abandons every workflow still
// pinned to it: those workflow tasks are correctly NOT dispatched to the new
// version, so the workflows stop making progress until a worker of their
// version exists again. A versioned worker therefore needs version-per-
// Deployment rollout — keep the old version running until Temporal reports it
// drained — not an in-place replacement.

// Env vars a versioned worker reads.
const (
	EnvDeploymentName = "TEMPORAL_WORKER_DEPLOYMENT_NAME"
	EnvBuildID        = "TEMPORAL_WORKER_BUILD_ID"
)

// WorkerOption customizes the worker NewWorker builds. Options are applied in
// order and then resolved as a set, so options of DIFFERENT kinds compose in any
// order. Repeating the same kind is last-wins — passing two identities or two
// behaviors is the caller choosing between them, and only there does order
// decide.
type WorkerOption func(*worker.Options)

// Versioning opts the worker into Worker Deployment Versioning under
// deploymentName/buildID. Workflows that register no behavior of their own
// default to Pinned — see the "Pinned carries a deployment contract" note above,
// and WithDefaultVersioningBehavior to change it.
//
// Identifiers are trimmed and validated here rather than deferred to the SDK,
// which accepts an empty Version at both worker.New and RegisterWorkflow (the
// registration guard at internal_worker.go:1232-1235 requires a non-zero
// Version) — the pod would reach Ready and then fail on every poll.
//
// Most workers read identifiers from the environment; use VersioningFromEnv.
func Versioning(deploymentName, buildID string) (WorkerOption, error) {
	name, id := strings.TrimSpace(deploymentName), strings.TrimSpace(buildID)
	if err := validateVersioning(name, id); err != nil {
		return nil, fmt.Errorf("temporalx: %w", err)
	}
	return withVersioning(name, id), nil
}

// MustVersioning is Versioning for identifiers that are program constants,
// panicking the way regexp.MustCompile does. Anything read at runtime — flags, a
// config file, a secret store — belongs on Versioning so the caller can report
// the failure in its own terms.
func MustVersioning(deploymentName, buildID string) WorkerOption {
	opt, err := Versioning(deploymentName, buildID)
	if err != nil {
		panic(err.Error())
	}
	return opt
}

// WithDefaultVersioningBehavior sets the behavior applied to workflows that
// register none of their own. Order relative to Versioning does not matter;
// repeating this option is last-wins.
//
// VersioningBehaviorUnspecified is a no-op, not a reset. The default cannot be
// removed — with versioning on the SDK panics at registration for a workflow
// that ends up with no behavior — and treating it as a reset would let a
// zero-valued config field silently downgrade an explicit AutoUpgrade to
// Pinned, leaving the old deployment version unable to drain.
//
// Any other value panics: it is a program constant, and an out-of-range
// behavior survives both worker.New and RegisterWorkflow only to panic inside
// the workflow task handler on the first behavior-less workflow task
// (versioningBehaviorToProto, internal/workflow.go:3180-3191).
func WithDefaultVersioningBehavior(b workflow.VersioningBehavior) WorkerOption {
	switch b {
	case workflow.VersioningBehaviorUnspecified:
		return func(*worker.Options) {}
	case workflow.VersioningBehaviorPinned, workflow.VersioningBehaviorAutoUpgrade:
		return func(o *worker.Options) {
			o.DeploymentOptions.DefaultVersioningBehavior = b
		}
	default:
		panic(fmt.Sprintf("temporalx.WithDefaultVersioningBehavior: unknown behavior %d", int(b)))
	}
}

// VersioningFromEnv builds the versioning option from EnvDeploymentName and
// EnvBuildID.
//
// Neither variable present ⇒ versioning is off and the option is a no-op, so a
// worker can call this unconditionally and let the manifests decide. Anything
// else is validated: presence decides, not emptiness, because an env var that
// exists but is empty (a YAML quoting slip, an empty ConfigMap key) would
// otherwise read as "unset" and silently produce an unversioned worker while
// the manifests, dashboards and rollout plan all say it is versioned.
//
// Remember that a successful return only means this worker polls as versioned —
// see the two-step note in this file's header.
func VersioningFromEnv() (WorkerOption, error) {
	rawName, nameSet := os.LookupEnv(EnvDeploymentName)
	rawBuild, buildSet := os.LookupEnv(EnvBuildID)
	name, buildID := strings.TrimSpace(rawName), strings.TrimSpace(rawBuild)

	if !nameSet && !buildSet {
		return func(*worker.Options) {}, nil
	}
	if err := validateVersioning(name, buildID); err != nil {
		return nil, fmt.Errorf("temporalx: worker versioning misconfigured: %w (%s=%q, %s=%q) — set both or neither",
			err, EnvDeploymentName, rawName, EnvBuildID, rawBuild)
	}
	return withVersioning(name, buildID), nil
}

// MustVersioningFromEnv is VersioningFromEnv, exiting on a misconfiguration the
// way flagx does (homelab ADR-029): a worker that looks versioned but polls
// unversioned would pick up work pinned elsewhere, which is worse than not
// starting. Use VersioningFromEnv to fold the error into a service's own
// startup validation.
func MustVersioningFromEnv() WorkerOption {
	opt, err := VersioningFromEnv()
	if err != nil {
		// slog, not log.Fatal: this package already logs through slog
		// (logMetricsError), and mixing sinks would split a worker's startup
		// story across two formats.
		slog.Error("temporalx: refusing to start", "error", err)
		os.Exit(1)
	}
	return opt
}

// withVersioning is the unvalidated constructor shared by the exported entry
// points. It sets identity only; the default behavior is resolved once, in
// normalizeVersioning, so option order cannot change the result.
func withVersioning(deploymentName, buildID string) WorkerOption {
	return func(o *worker.Options) {
		o.DeploymentOptions.UseVersioning = true
		o.DeploymentOptions.Version = worker.WorkerDeploymentVersion{
			DeploymentName: deploymentName,
			BuildID:        buildID,
		}
	}
}

func validateVersioning(deploymentName, buildID string) error {
	switch {
	case deploymentName == "":
		return errors.New("deployment name is empty")
	case buildID == "":
		return errors.New("build id is empty")
	case strings.Contains(deploymentName, "."):
		// The SDK joins the two into "<name>.<buildID>" and parses it back with
		// SplitN(version, ".", 2) (internal_worker.go:2839-2844), so a dotted
		// deployment name silently re-parses as a different identity: the
		// operator's set-current-version then targets a deployment that no
		// worker polls, and the ramp looks like it does nothing.
		return fmt.Errorf("deployment name %q contains '.', which the SDK reserves to join name and build id", deploymentName)
	}
	return nil
}

// normalizeVersioning resolves DeploymentOptions after every option has run, so
// the option set is order-insensitive, and reports the outcome — flagx logs its
// resolved value for the same reason: choosing the wrong mode must leave a
// startup trace, not be inferred from a stalled workflow hours later.
func normalizeVersioning(o *worker.Options) {
	d := &o.DeploymentOptions

	if !d.UseVersioning {
		// worker.New panics when a behavior is set without versioning
		// (internal_worker.go:2218-2223). Dropping it keeps a stray
		// WithDefaultVersioningBehavior from killing the process over a setting
		// that has no effect anyway.
		d.DefaultVersioningBehavior = workflow.VersioningBehaviorUnspecified
		slog.Info("temporalx: worker versioning off")
		return
	}

	// WorkerOption is an exported func type, so a hand-written option can set
	// DeploymentOptions directly and bypass the validated constructors. Both
	// worker.New and RegisterWorkflow ACCEPT an empty Version — the registration
	// guard at internal_worker.go:1232-1235 is gated on Version being non-zero,
	// so an empty one disarms it — and the worker would reach Ready and then
	// poll as versioned under an empty identity. This can never fire for
	// WithVersioning or VersioningFromEnv; it exists to close that escape hatch.
	if err := validateVersioning(d.Version.DeploymentName, d.Version.BuildID); err != nil {
		panic("temporalx: worker versioning enabled without a valid identity: " + err.Error())
	}

	switch d.DefaultVersioningBehavior {
	case workflow.VersioningBehaviorUnspecified:
		d.DefaultVersioningBehavior = workflow.VersioningBehaviorPinned
	case workflow.VersioningBehaviorPinned, workflow.VersioningBehaviorAutoUpgrade:
	default:
		// Same escape hatch as the identity check above: a hand-written option
		// can set an out-of-range behavior that survives worker.New AND
		// RegisterWorkflow, then panics in the workflow task handler on the
		// first behavior-less task (versioningBehaviorToProto,
		// internal/workflow.go:3180-3191). Fail here instead.
		panic(fmt.Sprintf("temporalx: unknown default versioning behavior %d", int(d.DefaultVersioningBehavior)))
	}
	slog.Info("temporalx: worker versioning on",
		"deployment_name", d.Version.DeploymentName,
		"build_id", d.Version.BuildID,
		"default_behavior", behaviorName(d.DefaultVersioningBehavior),
		"note", "polling as versioned only — the deployment's current version must be set server-side to route work here")
}

// behaviorName renders a behavior for logs: the SDK type has no String method,
// and an operator reading a startup line needs the name, not the enum ordinal.
// The fallback is unreachable: the option and normalizeVersioning both reject
// out-of-range values, so a log line can never render a bare ordinal.
func behaviorName(b workflow.VersioningBehavior) string {
	switch b {
	case workflow.VersioningBehaviorPinned:
		return "pinned"
	case workflow.VersioningBehaviorAutoUpgrade:
		return "auto-upgrade"
	default:
		return fmt.Sprintf("unknown(%d)", int(b))
	}
}
