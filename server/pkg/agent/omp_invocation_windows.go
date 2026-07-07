//go:build windows

package agent

import "log/slog"

// platformOmpInvocation rewrites omp.cmd → PowerShell -File omp.ps1 on
// Windows to avoid cmd.exe %* re-tokenisation (see #3306).
// powerShellLookup and rewriteCmdToPS1 are defined in cursor_invocation_windows.go.
func platformOmpInvocation(lookedUp string, args []string, logger *slog.Logger) (string, []string, bool) {
	return rewriteCmdToPS1("omp", lookedUp, args, logger)
}
