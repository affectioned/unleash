package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/VoidChecksum/unleash/internal/console"
	"github.com/VoidChecksum/unleash/internal/target"
)

// ccPkg is the npm package name for Claude Code.
const ccPkg = "@anthropic-ai/claude-code"

// ccInstall is one Claude Code installation slated for removal.
type ccInstall struct {
	Root    string // directory or file to delete
	Manager string // "npm","pnpm","bun","volta","winget","scoop","choco","brew-cask","native"
	ID      string // package manager id (winget only)
	Sample  string // discovered binary path this root was derived from
}

// NewUninstallCCCmd creates the "uninstall-cc" cobra command.
func NewUninstallCCCmd() *cobra.Command {
	var yes, purge, dryRun bool
	c := &cobra.Command{
		Use:     "uninstall-cc",
		Aliases: []string{"remove-cc", "nuke-cc"},
		Short:   "Uninstall every Claude Code installation on this machine",
		Long: `Uninstall all Claude Code installations found on this machine.

Discovers every install across npm/pnpm/bun/volta/nvm/fnm/mise, the native
installer, WinGet, Scoop, Chocolatey and Homebrew, runs each package
manager's own uninstall, then deletes any remaining install files.

Without --yes this only prints what would be removed (dry run).
Use --purge to also delete Claude Code user config (~/.claude, ~/.claude.json).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstallCC(yes, purge, dryRun)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Actually perform removal (without this it is a dry run)")
	c.Flags().BoolVar(&purge, "purge", false, "Also delete Claude Code user config and data")
	c.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "Preview only; never remove anything")
	return c
}

func runUninstallCC(yes, purge, dryRun bool) error {
	all := target.FindAllTargets()
	installs := collectCCInstalls(all)
	cfgPaths := ccConfigPaths()

	console.PrintBold("unleash uninstall-cc")

	if len(installs) == 0 && !(purge && anyExists(cfgPaths)) {
		console.PrintWarn("No Claude Code installations found.")
		return nil
	}

	if len(installs) > 0 {
		fmt.Printf("  %d installation(s):\n", len(installs))
		for _, ins := range installs {
			mgr := ins.Manager
			if ins.Manager == "native" {
				mgr = "native/manual"
			}
			fmt.Printf("    %s [%s]\n", ins.Root, mgr)
		}
	}
	if purge {
		existing := filterExisting(cfgPaths)
		if len(existing) > 0 {
			fmt.Printf("  config/data to purge:\n")
			for _, p := range existing {
				fmt.Printf("    %s\n", p)
			}
		}
	}

	if !yes || dryRun {
		console.PrintWarn("dry run — nothing removed. Re-run with --yes to uninstall.")
		return nil
	}

	// Phase 1: let each package manager uninstall itself (keeps its manifests tidy).
	for _, mgr := range orderedManagers(installs) {
		runManagerUninstall(mgr, installs)
	}

	// Phase 2: delete any install root still on disk.
	for _, ins := range installs {
		if _, err := os.Stat(ins.Root); os.IsNotExist(err) {
			continue // package manager already removed it
		}
		if err := os.RemoveAll(ins.Root); err != nil {
			console.PrintErr("remove %s: %v", ins.Root, err)
		} else {
			console.PrintOK("removed %s", ins.Root)
		}
	}

	// Phase 3: optionally purge user config/data.
	if purge {
		for _, p := range filterExisting(cfgPaths) {
			if err := os.RemoveAll(p); err != nil {
				console.PrintErr("purge %s: %v", p, err)
			} else {
				console.PrintOK("purged %s", p)
			}
		}
	}

	// Summary: rescan.
	remaining := target.FindAllTargets()
	if len(remaining) == 0 {
		console.PrintOK("All Claude Code installations removed.")
	} else {
		console.PrintWarn("%d installation(s) still detected:", len(remaining))
		for _, f := range remaining {
			console.PrintDot("%s", f.Path)
		}
	}
	return nil
}

// collectCCInstalls maps discovered binaries to deduplicated install roots,
// tagging each with the package manager that owns it. Roots nested under
// another root are dropped.
func collectCCInstalls(all []target.Found) []ccInstall {
	var raw []ccInstall
	for _, f := range all {
		root, mgr, id := ccInstallRoot(f.Path)
		raw = append(raw, ccInstall{Root: root, Manager: mgr, ID: id, Sample: f.Path})
	}

	// Dedupe by root, preferring the first (which also carries a manager id).
	seen := make(map[string]ccInstall)
	var order []string
	for _, ins := range raw {
		key := filepath.ToSlash(ins.Root)
		if _, ok := seen[key]; !ok {
			seen[key] = ins
			order = append(order, key)
		}
	}

	// Drop roots that live under another kept root.
	sort.Slice(order, func(i, j int) bool { return len(order[i]) < len(order[j]) })
	var kept []string
	for _, key := range order {
		nested := false
		for _, k := range kept {
			if key == k || strings.HasPrefix(key, k+"/") {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, key)
		}
	}

	out := make([]ccInstall, 0, len(kept))
	for _, key := range kept {
		out = append(out, seen[key])
	}
	return out
}

// ccInstallRoot derives the directory (or file) to delete for a discovered
// Claude Code binary, plus the owning package manager and, for WinGet, its
// package id. Paths are matched case-insensitively on a slash-normalized copy.
func ccInstallRoot(path string) (root, manager, id string) {
	np := filepath.ToSlash(path)
	lp := strings.ToLower(np)

	// Volta: remove the package image dir; `volta uninstall` clears the shim.
	if r, ok := cutAtMarker(np, lp, "/.volta/tools/image/packages/@anthropic-ai/claude-code"); ok {
		return filepath.FromSlash(r), "volta", ""
	}

	// Any npm-layout package (npm/pnpm/bun/nvm/fnm/mise/appdata/brew-node).
	if r, ok := cutAtMarker(np, lp, "node_modules/@anthropic-ai/claude-code"); ok {
		mgr := "npm"
		switch {
		case strings.Contains(lp, "/.bun/"):
			mgr = "bun"
		case strings.Contains(lp, "/pnpm/"):
			mgr = "pnpm"
		}
		return filepath.FromSlash(r), mgr, ""
	}

	// WinGet portable package dir.
	if r, ok := cutAfterPrefixSegment(np, lp, "microsoft/winget/packages/"); ok {
		return filepath.FromSlash(r), "winget", strings.SplitN(filepath.Base(r), "_", 2)[0]
	}

	// Scoop / Chocolatey / Homebrew cask.
	if r, ok := cutAtMarker(np, lp, "/scoop/apps/claude-code"); ok {
		return filepath.FromSlash(r), "scoop", ""
	}
	if r, ok := cutAtMarker(np, lp, "/chocolatey/lib/claude-code"); ok {
		return filepath.FromSlash(r), "choco", ""
	}
	if r, ok := cutAfterPrefixSegment(np, lp, "caskroom/"); ok {
		return filepath.FromSlash(r), "brew-cask", ""
	}

	// Native installer / manual layouts — no package manager, just delete.
	for _, m := range []string{
		"/.local/share/claude/versions",
		"/.local/share/claude-code",
		"/.claude/local",
		"/.claude/bin",
	} {
		// Trim the trailing "/versions" so the whole install dir is removed.
		marker := strings.TrimSuffix(m, "/versions")
		if r, ok := cutAtMarker(np, lp, marker); ok {
			return filepath.FromSlash(r), "native", ""
		}
	}
	if r, ok := cutAtMarker(np, lp, "programs/claude/versions"); ok {
		r = strings.TrimSuffix(r, "/versions")
		return filepath.FromSlash(r), "native", ""
	}
	for _, m := range []string{"programs/claude-code", "anthropic/claude-code"} {
		if r, ok := cutAtMarker(np, lp, m); ok {
			return filepath.FromSlash(r), "native", ""
		}
	}

	// Fallback: a standalone launcher — delete just the file.
	return path, "native", ""
}

// cutAtMarker returns np truncated to just after the first occurrence of
// marker (matched in the lowercased lp). ok is false when marker is absent.
func cutAtMarker(np, lp, marker string) (string, bool) {
	i := strings.Index(lp, marker)
	if i < 0 {
		return "", false
	}
	return np[:i+len(marker)], true
}

// cutAfterPrefixSegment finds prefix in lp, then returns np truncated at the
// end of the path segment that immediately follows prefix (i.e. prefix + one
// directory name). ok is false when prefix is absent.
func cutAfterPrefixSegment(np, lp, prefix string) (string, bool) {
	i := strings.Index(lp, prefix)
	if i < 0 {
		return "", false
	}
	start := i + len(prefix)
	if j := strings.IndexByte(np[start:], '/'); j >= 0 {
		return np[:start+j], true
	}
	return np, true
}

// orderedManagers returns the unique package managers present, in a stable order.
func orderedManagers(installs []ccInstall) []string {
	seen := make(map[string]bool)
	var out []string
	for _, order := range []string{"npm", "pnpm", "bun", "volta", "winget", "scoop", "choco", "brew-cask"} {
		for _, ins := range installs {
			if ins.Manager == order && !seen[order] {
				seen[order] = true
				out = append(out, order)
			}
		}
	}
	return out
}

// runManagerUninstall invokes a package manager's own uninstall command.
func runManagerUninstall(mgr string, installs []ccInstall) {
	var bin string
	var args []string
	switch mgr {
	case "npm":
		bin, args = "npm", []string{"uninstall", "-g", ccPkg}
	case "pnpm":
		bin, args = "pnpm", []string{"uninstall", "-g", ccPkg}
	case "bun":
		bin, args = "bun", []string{"remove", "-g", ccPkg}
	case "volta":
		bin, args = "volta", []string{"uninstall", ccPkg}
	case "scoop":
		bin, args = "scoop", []string{"uninstall", "claude-code"}
	case "choco":
		bin, args = "choco", []string{"uninstall", "claude-code", "-y"}
	case "brew-cask":
		bin, args = "brew", []string{"uninstall", "--cask", "claude-code"}
	case "winget":
		id := "Anthropic.ClaudeCode"
		for _, ins := range installs {
			if ins.Manager == "winget" && ins.ID != "" {
				id = ins.ID
				break
			}
		}
		bin, args = "winget", []string{"uninstall", "--id", id, "--silent",
			"--accept-source-agreements", "--disable-interactivity"}
	default:
		return
	}

	p, ok := lookPathExec(bin)
	if !ok {
		return // manager not on PATH — phase 2 file removal handles it
	}
	console.PrintInfo("%s %s", filepath.Base(p), strings.Join(args, " "))
	c := exec.Command(p, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		console.PrintWarn("%s uninstall exited: %v (continuing)", mgr, err)
	}
}

// lookPathExec resolves a binary on PATH, trying Windows extensions.
func lookPathExec(name string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".cmd", ".exe", ".bat"} {
			if p, err := exec.LookPath(name + ext); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

// ccConfigPaths returns Claude Code user config/data locations (--purge targets).
func ccConfigPaths() []string {
	home := homeDir()
	paths := []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude.json.backup"),
	}
	if isWindows() {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			paths = append(paths, filepath.Join(appdata, "claude"))
		}
	} else {
		paths = append(paths, filepath.Join(home, ".config", "claude"))
	}
	return paths
}

func anyExists(paths []string) bool {
	return len(filterExisting(paths)) > 0
}

func filterExisting(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
