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
// No build tag: the client lives behind one (ipc.go is Linux-only), but a
// tagless file keeps the package compiling on a non-Linux dev box exactly
// as capbroker.go does.
var tracer = otel.Tracer("cmd/capbroker")
