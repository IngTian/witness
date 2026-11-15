//go:build !linux

package proc

import "os/exec"

// BindToParent is a no-op on macOS/Windows: neither has a Pdeathsig equivalent in
// syscall.SysProcAttr (the field is Linux-only), so there is no way to bind the
// child's lifetime to this process at fork time. The `opencode serve` child does not
// need it: it ends on its stdin lease when this process dies, however it dies (issue
// #54 I2). On Windows a contained worker's `claude -p` children die with its
// kill-on-close job (ContainChildren in proc_windows.go).
func (sys) BindToParent(cmd *exec.Cmd) {}
