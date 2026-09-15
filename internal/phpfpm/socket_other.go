//go:build !linux

package phpfpm

// socketsMatch is Linux-only: PHP-FPM pools exist nowhere else this builds
// for. The stub keeps the package compiling on a workstation, where the tests
// that matter are the ones about rendering.
func socketsMatch([]Pool) bool { return true }
