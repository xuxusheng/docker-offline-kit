// dok-deploy — 把离线安装器直推到远程服务器并安装（阶段二 ②）
//
// 用法:
//
//	dok-deploy <ssh目标> [--installer <本地文件>] [-- <安装器参数>...]
//
// <ssh目标> 为 ~/.ssh/config 里的别名或 user@host；ssh/scp 的全部能力
// （跳板机、密钥、配置）原样复用。远端 sudo 密码交互通过 tty 透传。
//
// 流程: 探测架构 → 上传 → 远程 doctor 预检 → sudo 安装 → 远程验证
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var (
	installerPath string
	passthrough   []string
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[dok-deploy] "+f+"\n", a...)
	os.Exit(1)
}

// sshDefaults 非交互场景的合理默认：首次连接自动接受 host key
var sshDefaults = []string{"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10"}

func runssh(target string, remote string, tty bool) (int, error) {
	args := append([]string{}, sshDefaults...)
	if tty {
		args = append(args, "-t")
	}
	args = append(args, target, remote)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	return exitCode(err), err
}

func scpto(target, local, remote string) error {
	args := append([]string{}, sshDefaults...)
	return exec.Command("scp", append(args, local, target+":"+remote)...).Run()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	die("ssh/scp 执行失败: %v", err)
	return 1
}

// resolveInstaller 定位要推送的安装器：--installer > 最新构建的 universal > 报错
func resolveInstaller() string {
	if installerPath != "" {
		if _, err := os.Stat(installerPath); err != nil {
			die("--installer 指定的文件不存在: %v", err)
		}
		return installerPath
	}
	// 优先 release/ 下最新修改的 universal
	candidates, _ := filepath.Glob("release/dok-installer-*-universal.run")
	if len(candidates) == 0 {
		candidates, _ = filepath.Glob("release/dok-installer-latest-universal.run")
	}
	if len(candidates) == 0 {
		die("未找到本地安装器。请先 go run ./cmd/pack，或用 --installer 指定文件\n  （也可从 https://github.com/xuxusheng/docker-offline-kit/releases 下载）")
	}
	sort.Strings(candidates)
	return candidates[len(candidates)-1]
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(`用法: dok-deploy <ssh目标> [--installer <本地文件>] [-- <安装器参数>...]

  <ssh目标>              ~/.ssh/config 别名或 user@host
  --installer <文件>     指定本地安装器（默认取 release/ 下最新 universal）
  -- <安装器参数>        透传给远端安装器，如 --yes --mirror <url> --live-restore

示例:
  dok-deploy 9y2
  dok-deploy root@1.2.3.4 -- --yes --live-restore
  dok-deploy web01 --installer dist/installer-x86_64`)
		return
	}
	// 解析参数
	target := args[0]
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--installer":
			if i+1 >= len(rest) {
				die("--installer 需要参数")
			}
			installerPath = rest[i+1]
			i++
		case "--":
			passthrough = append(passthrough, rest[i+1:]...)
			i = len(rest)
		default:
			die("未知参数: %s（安装器参数请放在 -- 之后）", rest[i])
		}
	}

	local := resolveInstaller()
	base := filepath.Base(local)

	fmt.Printf("[dok-deploy] 安装器: %s\n", local)

	// 1) 连通性 + 架构探测
	fmt.Printf("[dok-deploy] 探测目标机 %s ...\n", target)
	code, err := runssh(target, "uname -m && (command -v docker >/dev/null && echo HASDOCKER || echo NODOCKER)", false)
	_ = code
	if err != nil {
		die("目标机连接失败: %v", err)
	}

	// 2) 上传
	fmt.Printf("[dok-deploy] 上传 %s ...\n", base)
	if err := scpto(target, local, "/tmp/"+base); err != nil {
		die("上传失败: %v", err)
	}

	// 3) 远端预检（doctor 失败则中止，exit 2）
	fmt.Println("[dok-deploy] 远端预检 ...")
	code, _ = runssh(target, "chmod 755 /tmp/"+base+" && bash /tmp/"+base+" doctor", false)
	if code == 2 {
		die("远端预检存在阻断项，已中止（详见上方报告）")
	}

	// 4) 远端安装（-t 透传 tty：sudo 密码/交互确认原样工作）
	fmt.Println("[dok-deploy] 远端安装 ...")
	remoteCmd := "sudo bash /tmp/" + base + " " + strings.Join(passthrough, " ")
	code, err = runssh(target, remoteCmd, true)
	if err != nil || code != 0 {
		die("远端安装失败（exit %d）", code)
	}

	// 5) 远端验证
	fmt.Println("[dok-deploy] 远端验证 ...")
	runssh(target, "docker version --format 'Docker {{.Server.Version}} ✓' && docker compose version", false)

	fmt.Println("[dok-deploy] ✓ 完成。目标机清理: ssh " + target + " rm -f /tmp/" + base)
}
