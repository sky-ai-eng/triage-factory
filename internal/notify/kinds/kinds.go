// Package kinds holds core's notification kinds, one file per source, so every
// audience and every line of copy is reviewed in one place. It imports notify
// and domain and never a producer package. A kind arrives with the producer
// that raises it; an extension defines its own in its own package.
package kinds
