//go:build !windows

package target

import "os/exec"

func hideConsole(_ *exec.Cmd) {}
