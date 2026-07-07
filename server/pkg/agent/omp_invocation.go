package agent

import "log/slog"

// chooseOmpInvocation selects the actual program (argv[0]) and the full
// argv to spawn an omp run.
//
// omp is installed via bun/npm, so on Windows the same .cmd-launcher
// re-tokenisation problem that affects Pi (#3306) applies: omp.cmd's body is
// "powershell ... -File omp.ps1 %*", and CreateProcess for a .cmd file goes
// through cmd.exe, where %* re-tokenises the command line and mangles
// multi-line arguments (notably the positional prompt). We resolve omp.ps1
// next to the .cmd and invoke PowerShell with `-File <ps1>` directly so Go
// passes each argv element as a discrete token.
//
// The Windows-specific behaviour is implemented in omp_invocation_windows.go;
// on other platforms we fall through to a passthrough (omp's binstub is a
// shebang script that execs bun directly, so argv passes through unchanged).
func chooseOmpInvocation(execName, lookedUp string, args []string, logger *slog.Logger) (string, []string) {
	if argv0, full, ok := platformOmpInvocation(lookedUp, args, logger); ok {
		return argv0, full
	}
	return execName, args
}
