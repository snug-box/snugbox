// Package safefs writes files as root without following a symbolic link at
// any step, rule 6 of blueprint section 3.7: every folder is opened with
// O_NOFOLLOW from the root down, names are resolved with openat2 and
// RESOLVE_NO_SYMLINKS where the kernel has it, new files are created with
// O_EXCL, and a replaced file is written to a temporary file in the same
// folder and renamed into place only if the target is a regular file.
//
// The package works on Linux only.
package safefs
