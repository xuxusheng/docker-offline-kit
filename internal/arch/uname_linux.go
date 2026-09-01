package arch

import (
	"os/exec"
	"runtime"
	"strings"
)

// unameM 通过 shell 获取 uname -m（安装器运行于 Linux，可直接依赖）。
func unameM() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		// 极端情况下退回 runtime.GOARCH
		return map[string]string{"amd64": "x86_64", "arm64": "aarch64", "arm": "armv7l"}[runtime.GOARCH]
	}
	return strings.TrimSpace(string(out))
}
