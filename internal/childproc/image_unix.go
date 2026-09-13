//go:build unix

package childproc

// ImageName cannot be answered portably here and is not needed: a unix pid
// file is removed by the process that wrote it, and the pid space is wide
// enough that a stale one naming a live process is not the everyday event it
// is on Windows. Reporting it as unreadable is the honest answer, and every
// caller treats that as "cannot rule it out".
func ImageName(int) (string, bool) { return "", false }

// IsImage says yes for the same reason.
func IsImage(int, string) bool { return true }
