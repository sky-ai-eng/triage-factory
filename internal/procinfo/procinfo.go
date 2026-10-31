// Package procinfo makes this process's privilege posture readable from
// outside it. SetTitle names the process so a security reviewer's `ps` shows
// the cap-broker / orchestrator / sidecar split at a glance, and
// EffectiveCaps and BoundingCaps decode its capability sets for the boot log,
// where a line reporting "CapEff=cap_sys_admin,cap_net_admin" or
// "CapEff=(empty)" turns "trust the code" into "read the logs."
//
// Nothing here makes a security decision or enforces anything. Renaming a
// process's comm needs no capability, and the capability readers only
// report. The actual capability drop happens at exec, in a process that
// never runs Go code (see docker/entrypoint.sh), because capabilities are
// per-thread and the Go runtime spawns threads before main() runs; by the
// time any of this code executes, the drop (or lack of one) has already
// happened at the kernel level.
package procinfo
