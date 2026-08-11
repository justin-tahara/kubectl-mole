// Package settle decides when a watched resource has settled: the controller
// has observed the current generation, kstatus reports Current, and the state
// has held continuously for the stability window. See DESIGN.md "Settle
// semantics". Built and tested before any failure signature.
package settle
