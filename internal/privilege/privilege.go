// Package privilege 实现三级提权降级：root → sudo → su。
package privilege

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// Mode 是探测出的提权方式。
type Mode int

const (
	ModeRoot Mode = iota // 已经是 root
	ModeSudoNOPass       // sudo 免密
	ModeSudoPass         // sudo 需要密码（交互输入或 --sudo-pass）
	ModeSu               // 无 sudo，用 su -c（需要 root 密码）
	ModeNone             // 无法提权
)

func (m Mode) String() string {
	switch m {
	case ModeRoot:
		return "root 直接执行"
	case ModeSudoNOPass:
		return "sudo（免密）"
	case ModeSudoPass:
		return "sudo（密码）"
	case ModeSu:
		return "su（root 密码）"
	}
	return "不可用"
}

// Detect 探测当前可用的提权路径。
func Detect() Mode {
	if os.Geteuid() == 0 {
		return ModeRoot
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		// sudo -n true 成功 → NOPASSWD
		if err := exec.Command("sudo", "-n", "true").Run(); err == nil {
			return ModeSudoNOPass
		}
		return ModeSudoPass
	}
	if _, err := exec.LookPath("su"); err == nil {
		return ModeSu
	}
	return ModeNone
}

// Runner 封装"以 root 身份执行命令"。
type Runner struct {
	Mode     Mode
	SudoPass string // ModeSudoPass 时使用；为空则交互输入
	NonInteractive bool
}

// NewRunner 根据 Mode 创建 Runner；SudoPass 为空且需要密码时交互读取。
func NewRunner(mode Mode, sudoPass string, nonInteractive bool) (*Runner, error) {
	r := &Runner{Mode: mode, SudoPass: sudoPass, NonInteractive: nonInteractive}
	if mode == ModeSudoPass && sudoPass == "" {
		if nonInteractive {
			return nil, fmt.Errorf("非交互模式下 sudo 需要密码，请用 --sudo-pass 提供")
		}
		fmt.Print("请输入 sudo 密码: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return nil, err
		}
		r.SudoPass = string(b)
	}
	return r, nil
}

// Run 以 root 权限执行 argv（不经过 shell）。
func (r *Runner) Run(argv ...string) error {
	cmd, err := r.cmd(argv...)
	if err != nil {
		return err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// CombinedOutput 以 root 权限执行并返回合并输出。
func (r *Runner) CombinedOutput(argv ...string) (string, error) {
	cmd, err := r.cmd(argv...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// QuietRun 静默执行（输出丢弃）。
func (r *Runner) QuietRun(argv ...string) error {
	cmd, err := r.cmd(argv...)
	if err != nil {
		return err
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// SudoStdin 返回一个以 root 执行 argv 的 cmd，调用者可写入 Stdin
// （用于 tee 写文件等场景）。
func (r *Runner) SudoStdin(argv ...string) (*exec.Cmd, error) {
	return r.cmd(argv...)
}

func (r *Runner) cmd(argv ...string) (*exec.Cmd, error) {
	switch r.Mode {
	case ModeRoot:
		return exec.Command(argv[0], argv[1:]...), nil
	case ModeSudoNOPass, ModeSudoPass:
		return r.sudoCmd(argv...)
	case ModeSu:
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		return exec.Command("su", "-c", strings.Join(quoted, " ")), nil
	}
	return nil, fmt.Errorf("无可用的提权路径")
}

func (r *Runner) sudoCmd(argv ...string) (*exec.Cmd, error) {
	if r.Mode == ModeSudoPass && r.SudoPass != "" {
		// sudo -S 从 stdin 读密码
		args := append([]string{"-S", "-p", "", argv[0]}, argv[1:]...)
		cmd := exec.Command("sudo", args...)
		cmd.Stdin = strings.NewReader(r.SudoPass + "\n")
		return cmd, nil
	}
	return exec.Command("sudo", argv...), nil
}
