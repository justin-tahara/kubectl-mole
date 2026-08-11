// Package signatures maps observed cluster state to named failure causes
// (ImagePullBackOff, CrashLoopBackOff, PodUnschedulable, ...). One file per
// signature; each is a small, isolated, independently testable detector.
// This is the primary contribution surface of the project.
package signatures
