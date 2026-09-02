// installer 是注入 payload 后编译出的目标机安装器。
// 交互式但克制：分阶段彩色进度 + 决策点确认；--yes/--non-interactive 保留全自动通道。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"

	"docker-offline-kit/internal/doctor"
	"docker-offline-kit/internal/installer"
	"docker-offline-kit/internal/privilege"
)

var (
	yes            bool
	nonInteractive bool
	sudoPass       string
	noSystemd      bool
	mirror         string
	liveRestore    bool
	skipDoctor     bool
)

var version = "dev"

// logFile 安装日志（/tmp/dok-install-*.log），失败排查用
var logFile *os.File

func writeLog(line string) {
	if logFile != nil {
		fmt.Fprintln(logFile, line)
	}
}

// 包级颜色函数（run 与 doctor 子命令共用）
var (
	colGreen  = color.New(color.FgGreen, color.Bold).SprintFunc()
	colYellow = color.New(color.FgYellow).SprintFunc()
	colRed    = color.New(color.FgRed, color.Bold).SprintFunc()
	colCyan   = color.New(color.FgCyan, color.Bold).SprintFunc()
	colGray   = color.New(color.Faint).SprintFunc()
)

func main() {
	root := &cobra.Command{
		Use:   "dok-installer",
		Short: "Docker + Compose 离线安装器（单文件，零依赖）",
		Version: version,
		RunE:  run,
	}
	root.Flags().BoolVar(&yes, "yes", false, "所有确认取默认值（覆盖升级=Y）")
	root.Flags().BoolVar(&nonInteractive, "non-interactive", false, "非交互模式：需要用户输入时直接报错（CI 用）")
	root.Flags().StringVar(&sudoPass, "sudo-pass", "", "sudo 密码（自动化用；注意可能留存 history/CI 日志）")
	root.Flags().BoolVar(&noSystemd, "no-systemd", false, "无 systemd 环境：nohup 方式启动")
	root.Flags().StringVar(&mirror, "mirror", "", "镜像加速地址，逗号分隔（写入 /etc/docker/daemon.json）")
	root.Flags().BoolVar(&liveRestore, "live-restore", false, "启用 live-restore：daemon 升级/重启期间容器保持运行")
	root.Flags().BoolVar(&skipDoctor, "skip-doctor", false, "跳过预检（不建议）")
	root.AddCommand(&cobra.Command{
		Use: "debug-extract <dst>", Args: cobra.ExactArgs(1), Hidden: true,
		RunE: func(c *cobra.Command, a []string) error { return installer.DebugExtract(a[0]) },
	})
	root.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "仅做环境预检，不安装",
		RunE: func(c *cobra.Command, a []string) error {
			fmt.Println(colCyan("docker-offline-kit")+" 环境预检", colGray("v"+version))
			report := doctor.Run()
			printReport(report, colGreen, colYellow, colRed, colGray)
			if report.Fails > 0 { os.Exit(2) }
			return nil
		},
	})
	if err := root.Execute(); err != nil {
		if logFile != nil {
			fmt.Fprintf(os.Stderr, "（完整日志: %s）\n", logFile.Name())
		}
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	green, yellow, red, cyan, gray := colGreen, colYellow, colRed, colCyan, colGray
	fmt.Println(cyan("docker-offline-kit")+" 安装器", gray("v"+version))
	logFile, _ = os.CreateTemp("", "dok-install-*.log")
	if logFile != nil {
		defer logFile.Close()
		writeLog("version=" + version)
		writeLog("args=" + strings.Join(os.Args[1:], " "))
	}

	// ---------- 预检 ----------
	if !skipDoctor {
		fmt.Println(yellow("▸ 环境预检"))
		report := doctor.Run()
		printReport(report, green, yellow, red, gray)
		fmt.Printf("  预检结论: %s\n", report.Verdict())
		if report.Fails > 0 {
			return fmt.Errorf("存在阻断项，中止安装")
		}
	}

	// ---------- 覆盖确认 ----------
	if p, err := execLookPath("docker"); err == nil && !yes {
		running := runningContainers() // 尽力统计运行中容器数
		prompt := "已存在 Docker，是否覆盖升级（镜像/容器数据保留）？"
		if running >= 0 {
			prompt = fmt.Sprintf("已存在 Docker，且有 %d 个容器正在运行（升级期间会短暂中断），是否覆盖升级（数据保留）？", running)
		}
		if nonInteractive {
			return fmt.Errorf("检测到已安装 docker（%s），非交互模式需要 --yes 才允许覆盖升级", p)
		}
		fmt.Print(yellow("? ") + prompt + " [Y/n] ")
		if !askYesDefaultYes() {
			fmt.Println("已取消")
			return nil
		}
	}

	// ---------- 提权 ----------
	mode := privilege.Detect()
	fmt.Printf("%s 提权: %s\n", yellow("▸"), mode)
	if mode == privilege.ModeNone {
		return fmt.Errorf("既非 root 也无 sudo/su 可用，无法安装；请让管理员处理")
	}
	runner, err := privilege.NewRunner(mode, sudoPass, nonInteractive)
	if err != nil {
		return err
	}

	// ---------- 安装 ----------
	ui := installer.UI{
		Step: func(title string, total int) {
			fmt.Printf("%s %s\n", yellow("▸"), cyan(title))
			writeLog("STEP " + title)
		},
		StepOK:  func() { fmt.Printf("  %s\n", green("✓ 完成")); writeLog("  OK") },
		StepErr: func(err error) {
			fmt.Printf("  %s %v\n", red("✗ 失败"), err)
			writeLog("  FAIL: " + err.Error())
		},
		Info: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			fmt.Printf("    %s\n", msg)
			writeLog("  " + msg)
		},
		Warn: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			fmt.Printf("    %s\n", yellow(msg))
			writeLog("  WARN: " + msg)
		},
		AskYesNo: func(prompt string) bool {
			if yes {
				return true
			}
			if nonInteractive {
				return false
			}
			fmt.Printf("  %s %s [y/N] ", yellow("?"), prompt)
			return askYesDefaultNo()
		},
	}

	// 总进度条（步骤粒度，total 由 installer.Run 的 Step 回调提供）
	var pb *progressbar.ProgressBar
	stepsDone := 0
	baseStep := ui.Step
	ui.Step = func(title string, total int) {
		baseStep(title, total)
		pb = progressbar.NewOptions(total,
			progressbar.OptionSetDescription("总进度"),
			progressbar.OptionShowCount(),
			progressbar.OptionSetPredictTime(false),
			progressbar.OptionFullWidth(),
			progressbar.OptionSetRenderBlankState(true),
			progressbar.OptionOnCompletion(func() { fmt.Println() }),
		)
	}
	baseStepOK := ui.StepOK
	ui.StepOK = func() {
		baseStepOK()
		stepsDone++
		if pb != nil {
			pb.Set(stepsDone)
		}
	}

	fmt.Println(yellow("▸ 安装"))
	if err := installer.Run(runner, installer.Options{
		Mirror:         mirror,
		LiveRestore:    liveRestore,
		NoSystemd:      noSystemd,
		NonInteractive: nonInteractive,
		Yes:            yes,
	}, ui); err != nil {
		return err
	}
	if pb != nil {
		pb.Finish()
	}

	fmt.Println(green("\n✓ 安装完成"))
	if os.Geteuid() != 0 {
		fmt.Printf("  下一步: %s\n", gray("sudo usermod -aG docker $USER && 重新登录，然后即可 docker compose up 部署项目"))
	}
	return nil
}

func askYesDefaultYes() bool {
	var in string
	fmt.Scanln(&in)
	in = strings.TrimSpace(strings.ToLower(in))
	return in == "" || in == "y" || in == "yes"
}

func askYesDefaultNo() bool {
	var in string
	fmt.Scanln(&in)
	in = strings.TrimSpace(strings.ToLower(in))
	return in == "y" || in == "yes"
}

func execLookPath(name string) (string, error) {
	return lookPath(name)
}

func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// runningContainers 尽力统计运行中容器数（-1 表示无法统计，提示中省略）。
func runningContainers() int {
	out, err := exec.Command("sh", "-c", "docker ps -q 2>/dev/null | wc -l").Output()
	if err != nil {
		return -1
	}
	n := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return -1
	}
	return n
}


func printReport(report *doctor.Report, green, yellow, red, gray func(...any) string) {
	defer func() {
		if logFile != nil {
			writeLog(fmt.Sprintf("doctor: fails=%d warns=%d", report.Fails, report.Warns))
		}
	}()
	for _, c := range report.Checks {
		switch c.Status {
		case doctor.OK:
			fmt.Printf("  %s %-12s %s\n", green("✓"), c.Name, c.Detail)
		case doctor.WARN:
			line := fmt.Sprintf("  %s %-12s %s", yellow("⚠"), c.Name, c.Detail)
			if c.Fix != "" {
				line += "  " + gray("("+c.Fix+")")
			}
			fmt.Println(line)
		case doctor.FAIL:
			fmt.Printf("  %s %-12s %s\n", red("✗"), c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Printf("    %s %s\n", gray("→"), c.Fix)
			}
		}
	}
}
