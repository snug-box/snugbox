// Package kernel parses the text files Linux exposes under /proc and in the
// cgroup v2 filesystem.
//
// Every parser takes the file's contents and never touches the filesystem, so
// the same code runs against a live machine and against recorded fixtures. The
// parsers are fuzzed. A line a newer or patched kernel prints in an unexpected
// shape is skipped and counted, or reported as an error, and never panics.
//
// Sizes the kernel prints in kB are converted to bytes. The kernel's kB is
// 1024 bytes.
package kernel
