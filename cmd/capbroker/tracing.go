//go:build linux

package capbroker

import "go.opentelemetry.io/otel"

// tracer owns this package's spans. Conventions in internal/telemetry.
//
// It has exactly one user: IPCClient.callWithCap, which runs in the
// ORCHESTRATOR. The broker half of this package — Handle, the server, every
// privileged operation it dispatches — starts no span and never will, per
// the epic's standing decision that the most privileged process on the host
// gains no outbound telemetry dependency. The broker process also installs
// no TracerProvider, so even this var resolves to the global no-op there.
//
// Linux-only, like its one user: off Linux nothing references it.
var tracer = otel.Tracer("cmd/capbroker")
