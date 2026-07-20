//go:build !unix

package runjournal

func processAlive(pid int) bool {
	// Non-unix: cannot probe; treat unknown as dead so lock can be adopted.
	_ = pid
	return false
}
