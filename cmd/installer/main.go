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
	skipDoctor     bool
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "docker-offline-installer",
		Short: "Docker + Compose 离线安装器（单文件，零依赖）",
		Version: version,
		RunE:  run,
	}
	root.Flags().BoolVar(&yes, "yes", false, "所有确认取默认值（覆盖升级=Y）")
	root.Flags().BoolVar(&nonInteractive, "non-interactive", false, "非交互模式：需要用户输入时直接报错（CI 用）")
	root.Flags().StringVar(&sudoPass, "sudo-pass", "", "sudo 密码（自动化用；注意可能留存 history/CI 日志）")
	root.Flags().BoolVar(&noSystemd, "no-systemd", false, "无 systemd 环境：nohup 方式启动")
	root.Flags().StringVar(&mirror, "mirror", "", "镜像加速地址，逗号分隔（写入 /etc/docker/daemon.json）")
	root.Flags().BoolVar(&skipDoctor, "skip-doctor", false, "跳过预检（不建议）")
	root.AddCommand(&cobra.Command{
		Use: "debug-extract <dst>", Args: cobra.ExactArgs(1), Hidden: true,
		RunE: func(c *cobra.Command, a []string) error { return installer.DebugExtract(a[0]) },
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	green := color.New(color.FgGreen, color.Bold).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	red := color.New(color.FgRed, color.Bold).SprintFunc()
	cyan := color.New(color.FgCyan, color.Bold).SprintFunc()
	gray := color.New(color.Faint).SprintFunc()

	fmt.Println(cyan("docker-offline-kit")+" 安装器", gray("v"+version))

	// ---------- 预检 ----------
	if !skipDoctor {
		fmt.Println(yellow("▸ 环境预检"))
		report := doctor.Run()
		for _, c := range report.Checks {
			switch c.Status {
			case doctor.OK:
				fmt.Printf("  %s %-12s %s\n", green("✓"), c.Name, c.Detail)
			case doctor.WARN:
				fmt.Printf("  %s %-12s %s", yellow("⚠"), c.Name, c.Detail)
				if c.Fix != "" {
					fmt.Printf("  %s", gray("("+c.Fix+")"))
				}
				fmt.Println()
			case doctor.FAIL:
				fmt.Printf("  %s %-12s %s\n", red("✗"), c.Name, c.Detail)
				if c.Fix != "" {
					fmt.Printf("    %s %s\n", gray("→"), c.Fix)
				}
			}
		}
		fmt.Printf("  预检结论: %s\n", report.Verdict())
		if report.Fails > 0 {
			return fmt.Errorf("存在阻断项，中止安装")
		}
	}

	// ---------- 覆盖确认 ----------
	if p, err := execLookPath("docker"); err == nil && !yes {
		if nonInteractive {
			return fmt.Errorf("检测到已安装 docker（%s），非交互模式需要 --yes 才允许覆盖升级", p)
		}
		fmt.Print(yellow("? ") + "已存在 Docker，是否覆盖升级（镜像/容器数据保留）？[Y/n] ")
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
		},
		StepOK:  func() { fmt.Printf("  %s\n", green("✓ 完成")) },
		StepErr: func(err error) { fmt.Printf("  %s %v\n", red("✗ 失败"), err) },
		Info:    func(f string, a ...any) { fmt.Printf("    %s\n", fmt.Sprintf(f, a...)) },
		Warn:    func(f string, a ...any) { fmt.Printf("    %s\n", yellow(fmt.Sprintf(f, a...))) },
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

	// 总进度条（步骤粒度）
	pb := progressbar.NewOptions(100,
		progressbar.OptionSetDescription("总进度"),
		progressbar.OptionShowCount(),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionOnCompletion(func() { fmt.Println() }),
	)
	stepsDone := 0
	baseStepOK := ui.StepOK
	ui.StepOK = func() {
		baseStepOK()
		stepsDone++
		pb.Set(stepsDone * 100 / 8)
	}

	fmt.Println(yellow("▸ 安装"))
	if err := installer.Run(runner, installer.Options{
		Mirror:         mirror,
		NoSystemd:      noSystemd,
		NonInteractive: nonInteractive,
		Yes:            yes,
	}, ui); err != nil {
		return err
	}
	pb.Finish()

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
