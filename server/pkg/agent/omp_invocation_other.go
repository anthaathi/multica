//go:build !windows

package agent

import "log/slog"

// platformOmpInvocation is a no-op on non-Windows platforms: omp's binstub
// invokes bun directly via shebang and Go's os/exec can pass argv unchanged.
func platformOmpInvocation(_ string, _ []string, _ *slog.Logger) (string, []string, bool) {
	return "", nil, false
}
