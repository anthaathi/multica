package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinVersions defines the minimum required CLI version for each agent type.
// Versions below these will be rejected during daemon registration.
var MinVersions = map[string]string{
	"claude":  "2.0.0",
	"codex":   "0.100.0", // app-server --listen stdio:// added in 0.100.0
	"copilot": "1.0.0",   // --output-format json envelope stable from 1.0.x
	"grok":    "0.2.89",  // ACP + authenticate/session-load/set_model/MCP and --effort thinking flag
	"qwen":    "0.20.0",  // stream-json protocol captured and verified against Qwen Code 0.20.0
}

// MinHandoffCLIVersion is the lowest multica CLI version whose daemon renders
// the assignment handoff note into the run's opening prompt + issue_context.md
// (MUL-3375). Unlike quick-create this is a SOFT gate: assigning an issue with
// a note never fails on an old daemon — the assignment still takes effect, the
// note is simply dropped. The frontend reads HandoffSupported to gray out the
// note box and warn the user, so they aren't surprised by a silently ignored
// note. Bump this to the release that actually ships the daemon rendering.
const MinHandoffCLIVersion = "0.3.28"

// HandoffSupported reports whether a daemon reporting cliVersion is new enough
// to render handoff notes. Reuses the shared semver parsing + git-describe
// dev-build exemption but never errors — a missing/old/unparsable version
// simply means "not supported", which the soft gate degrades gracefully.
func HandoffSupported(cliVersion string) bool {
	d := strings.TrimSpace(cliVersion)
	if d == "" {
		return false
	}
	if devDescribeRe.MatchString(d) {
		return true
	}
	parsed, err := parseSemver(d)
	if err != nil {
		return false
	}
	min, err := parseSemver(MinHandoffCLIVersion)
	if err != nil {
		return false
	}
	return !parsed.lessThan(min)
}

// devDescribeRe matches the `git describe --tags --always --dirty` output for
// a build past the latest tag, e.g. `v0.2.15-235-gdaf0e935` (optionally with a
// trailing `-dirty`). Daemons built from source (Makefile `make build` / `make
// daemon`) report this shape; tagged releases are bare semver. Treating dev-
// described daemons as OK keeps `make daemon` unblocked without weakening the
// gate for staging or production users running stale stable releases.
var devDescribeRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+-\d+-g[0-9a-fA-F]+`)

// semver holds a parsed semantic version (major.minor.patch).
type semver struct {
	Major, Minor, Patch int
}

// versionRe matches version strings like "2.1.100", "v2.0.0", or
// "2.1.100 (Claude Code)" — it extracts the first three numeric components.
var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// parseSemver extracts a semver from a version string.
func parseSemver(raw string) (semver, error) {
	m := versionRe.FindStringSubmatch(raw)
	if m == nil {
		return semver{}, fmt.Errorf("cannot parse version %q", raw)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return semver{Major: major, Minor: minor, Patch: patch}, nil
}

// lessThan returns true if v < other.
func (v semver) lessThan(other semver) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	return v.Patch < other.Patch
}

// CheckMinVersion validates that detectedVersion meets the minimum for agentType.
// Returns nil if the version is acceptable or no minimum is defined.
func CheckMinVersion(agentType, detectedVersion string) error {
	minRaw, ok := MinVersions[agentType]
	if !ok {
		return nil
	}
	min, err := parseSemver(minRaw)
	if err != nil {
		return fmt.Errorf("invalid minimum version %q for %s: %w", minRaw, agentType, err)
	}
	detected, err := parseSemver(detectedVersion)
	if err != nil {
		return fmt.Errorf("cannot parse detected %s version %q: %w", agentType, detectedVersion, err)
	}
	if detected.lessThan(min) {
		return fmt.Errorf("%s version %s is below minimum required %s — please upgrade", agentType, detectedVersion, minRaw)
	}
	return nil
}
