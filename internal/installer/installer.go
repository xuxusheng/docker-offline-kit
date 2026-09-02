// Package installer 执行安装流水线：停旧 → 装二进制 → compose → systemd/nohup → 自验。
// payload 通过 go:embed 注入（pack 按架构分目录编译）。
package installer

import (
	"archive/tar"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"docker-offline-kit/internal/privilege"
)

//go:embed all:payload
var payloadFS embed.FS

// Options 安装参数。
type Options struct {
	Mirror         string // registry-mirrors，逗号分隔；空则不写 daemon.json
	LiveRestore    bool   // live-restore：daemon 重启/升级期间容器继续运行
	Rootless       bool   // rootless 模式：装到 ~/.local，用户态 daemon
	NoSystemd      bool
	NonInteractive bool
	Yes            bool // 所有确认取默认
}

// UI 是显示层回调，main 包注入（交互模式为彩色输出，非交互为纯文本）。
type UI struct {
	Step    func(title string, total int)
	StepOK  func()
	StepErr func(err error)
	Info    func(format string, a ...any)
	Warn    func(format string, a ...any)
	AskYesNo func(prompt string) bool // 返回 true=默认 Y
}

// HasPayload 报告 payload 是否已注入（开发构建可能为空）。
func HasPayload() bool {
	entries, err := payloadFS.ReadDir("payload")
	if err != nil || len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}

// Run 执行完整安装。ui 不能为 nil。
// 覆盖升级失败时自动回滚：恢复备份的旧版二进制并尽力重启旧服务。
func Run(root *privilege.Runner, opt Options, ui UI) error {
	// 前置：payload 存在
	if !HasPayload() {
		return fmt.Errorf("安装器内未发现内置 payload（开发构建？请用 make build 出正式包）")
	}

	if opt.Rootless {
		return runRootless(opt, ui)
	}
	if root == nil {
		return fmt.Errorf("内部错误：非 rootless 模式需要提权 Runner")
	}

	steps := []string{
		"停止旧服务", "解包引擎", "安装二进制", "安装 compose 插件",
	}
	if opt.NoSystemd {
		steps = append(steps, "nohup 启动 dockerd")
	} else {
		steps = append(steps, "注册 systemd 服务", "启动服务")
	}
	if opt.Mirror != "" || opt.LiveRestore {
		steps = append(steps, "写入 daemon.json")
	}
	steps = append(steps, "自验")

	ui.Step("安装", len(steps))
	done := func() { ui.StepOK() }
	fail := func(err error) error { ui.StepErr(err); return err }

	// 1) 停旧
	if err := stopOld(root, ui); err != nil {
		return fail(err)
	}
	done()

	// 2) 解包 payload 到临时目录
	tmp, err := os.MkdirTemp("", "dok-payload-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	ui.Info("解包内置 payload ...")
	if err := extractPayload(tmp); err != nil {
		return fail(err)
	}
	done()

	// 3) 备份旧版二进制 + compose（回滚用）
	binList := []string{"dockerd", "docker", "containerd", "containerd-shim-runc-v2", "runc", "ctr", "docker-init", "docker-proxy"}
	backupDir, hadOld := backupOld(root, binList)
	rollback := func(installErr error) error {
		if !hadOld {
			ui.Warn("本次为全新安装，无旧版可回滚；机器上尚无可用的 Docker，请根据上方错误排查后重试")
			return installErr
		}
		ui.Warn("安装失败，正在自动回滚到旧版 Docker ...")
		if rerr := restoreBackup(root, backupDir, binList); rerr != nil {
			ui.Warn("回滚失败：%v（备份保留在 %s，可手动恢复）", rerr, backupDir)
			return installErr
		}
		restartDockerd(root, opt.NoSystemd)
		if werr := waitDaemon(root, 60); werr != nil {
			ui.Warn("旧版已恢复但 daemon 未就绪：%v", werr)
		} else {
			ui.Warn("已回滚到旧版 Docker，服务已恢复 ✓")
		}
		return fmt.Errorf("%w（已回滚；备份保留在 %s）", installErr, backupDir)
	}

	// 3.5) docker 用户组（非 root 使用的前提；-f 组已存在不报错）
	root.QuietRun("groupadd", "-f", "docker")
	// 把"实际使用 docker 的用户"加进 docker 组（装完即用，免 usermod+重新登录）
	// - root 直接运行：无（root 本身可用）
	// - sudo 运行（euid=0）：SUDO_USER 即发起人
	// - 非 root 运行（sudo -u xxx / su 嵌套）：当前进程属主才是受益人
	target := ""
	if os.Geteuid() == 0 {
		if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
			target = u
		}
	} else if u, uerr := user.Current(); uerr == nil && u.Username != "root" {
		target = u.Username
	}
	if target != "" {
		if err := root.QuietRun("usermod", "-aG", "docker", target); err == nil {
			ui.Info("已将用户 %s 加入 docker 组", target)
		}
	}

	// 4) 安装二进制
	root.QuietRun("mkdir", "-p", "/usr/local/bin")
	for _, b := range binList {
		src := filepath.Join(tmp, "bin", b)
		if fileExists(src) {
			if err := root.Run("install", "-m", "0755", src, "/usr/local/bin/"+b); err != nil {
				return fail(rollback(fmt.Errorf("安装 %s: %w", b, err)))
			}
		}
	}
	done()

	// 5) compose 插件（多路径覆盖新旧 CLI 查找）
	if err := root.Run("mkdir", "-p", "/usr/local/lib/docker/cli-plugins", "/usr/libexec/docker/cli-plugins", "/usr/lib/docker/cli-plugins"); err != nil {
		return fail(rollback(err))
	}
	if err := root.Run("install", "-m", "0755", filepath.Join(tmp, "compose", "docker-compose"), "/usr/local/lib/docker/cli-plugins/docker-compose"); err != nil {
		return fail(rollback(err))
	}
	for _, d := range []string{"/usr/libexec/docker/cli-plugins", "/usr/lib/docker/cli-plugins"} {
		root.QuietRun("ln", "-sf", "/usr/local/lib/docker/cli-plugins/docker-compose", d+"/docker-compose")
	}
	done()

	// 6) systemd 或 nohup
	startErr := func() error {
		if opt.NoSystemd {
			if err := root.Run("mkdir", "-p", "/etc/docker", "/var/log"); err != nil {
				return err
			}
			if err := startNohup(root); err != nil {
				return err
			}
			ui.Warn("nohup 方式无开机自启；请将下行加入 /etc/rc.local:")
			ui.Warn("  nohup /usr/local/bin/dockerd >> /var/log/dockerd.log 2>&1 &")
			return nil
		}
		if err := writeSystemdUnits(root); err != nil {
			return err
		}
		if err := root.Run("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemd daemon-reload 失败（无 systemd？可改用 --no-systemd）: %w", err)
		}
		root.QuietRun("systemctl", "enable", "--now", "containerd")
		if err := root.Run("systemctl", "enable", "--now", "docker"); err != nil {
			return fmt.Errorf("启动 docker.service 失败: %w（可用 journalctl -u docker 查看）", err)
		}
		return nil
	}()
	if startErr != nil {
		return fail(rollback(startErr))
	}
	done()

	// 7) daemon.json（镜像加速/live-restore）：写配置后按启动方式正确重启
	if opt.Mirror != "" || opt.LiveRestore {
		if err := writeDaemonJSON(root, opt.Mirror, opt.LiveRestore, ui); err != nil {
			return fail(rollback(err))
		}
		if err := restartDockerd(root, opt.NoSystemd); err != nil {
			return fail(rollback(err))
		}
		done()
	}

	// 8) 等待 dockerd 就绪（nohup/systemd 启动都是异步的）
	ui.Info("等待 dockerd 就绪 ...")
	if err := waitDaemon(root, 60); err != nil {
		return fail(rollback(err))
	}

	// 9) 自验
	out, err := root.CombinedOutput("/usr/local/bin/docker", "version", "--format", "Docker {{.Server.Version}}")
	if err != nil {
		return fail(rollback(fmt.Errorf("自验失败，dockerd 未就绪: %w\n%s", err, out)))
	}
	ui.Info("Docker %s ✓", strings.TrimSpace(out))
	out, err = root.CombinedOutput("/usr/local/bin/docker", "compose", "version")
	if err != nil {
		return fail(rollback(fmt.Errorf("compose 插件验证失败: %w", err)))
	}
	ui.Info("%s ✓", strings.TrimSpace(out))
	done()

	// 成功：清理备份
	if hadOld {
		root.QuietRun("rm", "-rf", backupDir)
	}
	return nil
}

// backupOld 把旧版二进制与 compose 插件复制到备份目录；返回备份目录与是否存在旧版。
func backupOld(root *privilege.Runner, bins []string) (string, bool) {
	ts := timeNowStamp()
	dir := "/usr/local/bin/.dok-backup-" + ts
	found := false
	for _, b := range bins {
		if root.QuietRun("test", "-f", "/usr/local/bin/"+b) == nil {
			found = true
			root.QuietRun("mkdir", "-p", dir)
			root.QuietRun("cp", "-a", "/usr/local/bin/"+b, dir+"/")
		}
	}
	if root.QuietRun("test", "-f", "/usr/local/lib/docker/cli-plugins/docker-compose") == nil {
		found = true
		root.QuietRun("mkdir", "-p", dir+"/compose")
		root.QuietRun("cp", "-a", "/usr/local/lib/docker/cli-plugins/docker-compose", dir+"/compose/")
	}
	if found {
		root.QuietRun("mkdir", "-p", dir) // 仅有 compose 时兜底建目录
	}
	return dir, found
}

// restoreBackup 从备份目录恢复旧版。
func restoreBackup(root *privilege.Runner, backupDir string, bins []string) error {
	for _, b := range bins {
		if root.QuietRun("test", "-f", backupDir+"/"+b) == nil {
			if err := root.Run("install", "-m", "0755", backupDir+"/"+b, "/usr/local/bin/"+b); err != nil {
				return err
			}
		}
	}
	if root.QuietRun("test", "-f", backupDir+"/compose/docker-compose") == nil {
		if err := root.Run("install", "-m", "0755", backupDir+"/compose/docker-compose", "/usr/local/lib/docker/cli-plugins/docker-compose"); err != nil {
			return err
		}
	}
	return nil
}

// startNohup 拉起 dockerd（nohup 方式）。
func startNohup(root *privilege.Runner) error {
	if err := root.Run("mkdir", "-p", "/etc/docker", "/var/log"); err != nil {
		return err
	}
	return root.Run("bash", "-c", "nohup /usr/local/bin/dockerd >> /var/log/dockerd.log 2>&1 & disown")
}

// restartDockerd 按启动方式正确重启（修复 nohup+mirror 组合下配置不生效的问题）。
func restartDockerd(root *privilege.Runner, noSystemd bool) error {
	if noSystemd {
		root.QuietRun("pkill", "-x", "dockerd")
		sleep(root, 2)
		return startNohup(root)
	}
	return root.Run("systemctl", "restart", "docker")
}

// timeNowStamp 当前时间的文件名安全戳。
func timeNowStamp() string { return time.Now().Format("20060102-150405") }

// RunRootless 是 rootless 安装入口（main 包调用）。
func RunRootless(opt Options, ui UI) error { return runRootless(opt, ui) }

// runRootless 无特权安装：~/.local/bin + rootlesskit 用户态 daemon + systemd --user。
func runRootless(opt Options, ui UI) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("rootless 模式应以普通用户身份运行（当前为 root）——root 运行请去掉 --rootless")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	steps := []string{"解包引擎", "安装二进制到 ~/.local/bin", "rootless 前置检查", "配置 systemd --user 服务", "启动并验证"}
	ui.Step("rootless 安装", len(steps))
	done := func() { ui.StepOK() }
	fail := func(err error) error { ui.StepErr(err); return err }

	tmp, err := os.MkdirTemp("/var/tmp", "dok-payload-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	if err := extractPayload(tmp); err != nil {
		return fail(err)
	}
	done()

	// 安装引擎 + rootless 组件到 ~/.local/bin
	os.MkdirAll(binDir, 0755)
	for _, sub := range []string{"bin", "rootless"} {
		entries, _ := os.ReadDir(filepath.Join(tmp, sub))
		for _, e := range entries {
			src := filepath.Join(tmp, sub, e.Name())
			dst := filepath.Join(binDir, e.Name())
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				return fail(rerr)
			}
			if werr := os.WriteFile(dst, data, 0755); werr != nil {
				return fail(werr)
			}
		}
	}
	ui.Info("已安装到 %s（请确认其在 PATH 中）", binDir)
	done()

	// 前置检查：uidmap、subuid、userns
	ui.Info("rootless 前置检查 ...")
	for _, b := range []string{"newuidmap", "newgidmap"} {
		if _, lerr := exec.LookPath(b); lerr != nil {
			return fail(fmt.Errorf("缺 %s（请管理员安装 uidmap 包）", b))
		}
	}
	if n := usernsMax(); n <= 0 {
		return fail(fmt.Errorf("user namespaces 未启用（max_user_namespaces=%d）", n))
	}
	if !subuidHasUser() {
		ui.Warn("/etc/subuid 中没有当前用户条目——需管理员一次性执行:")
		ui.Warn("  sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s", os.Getenv("USER"))
		return fail(fmt.Errorf("缺 subuid/subgid 映射（管理员一次性配置后重试）"))
	}
	done()

	// 配置环境 + setup tool
	setup := filepath.Join(binDir, "dockerd-rootless-setuptool.sh")
	if _, serr := os.Stat(setup); serr != nil {
		return fail(fmt.Errorf("包内缺 dockerd-rootless-setuptool.sh"))
	}
	uid := fmt.Sprint(os.Getuid())
	os.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		xdg = "/run/user/" + uid
	}
	if st, serr := os.Stat(xdg); serr != nil || !st.IsDir() {
		return fail(fmt.Errorf(
			"未检测到用户运行时目录 %s——rootless 的 systemd --user 依赖 logind 用户会话；请通过 SSH 直接登录该用户后再试（sudo/su 切换不会创建会话）", xdg))
	}
	os.Setenv("XDG_RUNTIME_DIR", xdg)
	if _, berr := os.Stat(filepath.Join(xdg, "bus")); berr != nil {
		return fail(fmt.Errorf(
			"未检测到 systemd 用户总线 %s/bus——rootless 需要该用户的 systemd user session；请通过 SSH 直接登录该用户（或由管理员执行 systemctl start user@<uid>）后再试", xdg))
	}
	// 禁用 host 网络（rootlesskit 需 slirp4netns），setup tool 自检
	ui.Info("执行 dockerd-rootless-setuptool.sh install ...")
	cmd := exec.Command(setup, "install")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("rootless setup 失败: %w（常见原因见上方输出）", err))
	}
	done()

	// 验证（rootless context）
	verify := exec.Command(binDir+"/docker", "--context", "rootless", "version", "--format", "Docker {{.Server.Version}} (rootless)")
	out, verr := verify.CombinedOutput()
	if verr != nil {
		return fail(fmt.Errorf("rootless 引擎验证失败: %w\n%s", verr, out))
	}
	ui.Info("%s ✓", strings.TrimSpace(string(out)))
	done()

	ui.Warn("rootless 限制: 端口 <1024 需 setcap、cgroup 限额不生效、网络走 slirp4netns")
	ui.Warn("开机自启: 管理员执行一次 sudo loginctl enable-linger $USER 后 systemctl --user enable docker")
	return nil
}

func usernsMax() int {
	b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return -1
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	return n
}

func subuidHasUser() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		b, rerr := os.ReadFile(f)
		if rerr != nil || !strings.Contains(string(b), u.Username+":") {
			return false
		}
	}
	return true
}

func stopOld(root *privilege.Runner, ui UI) error {
	haveSystemd := dirExists("/run/systemd/system")
	if haveSystemd {
		for _, s := range []string{"docker", "docker.socket", "containerd"} {
			if err := root.QuietRun("systemctl", "stop", s); err == nil {
				ui.Info("已停止 %s", s)
			}
		}
	}
	// 兜底：残留 dockerd 进程
	if out, err := root.CombinedOutput("pkill", "-x", "dockerd"); err == nil && strings.TrimSpace(out) == "" {
		sleep(root, 2)
	}
	return nil
}

func sleep(root *privilege.Runner, sec int) { root.QuietRun("sleep", fmt.Sprint(sec)) }

func writeSystemdUnits(root *privilege.Runner) error {
	containerdUnit := `[Unit]
Description=containerd container runtime
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity

[Install]
WantedBy=multi-user.target
`
	dockerUnit := `[Unit]
Description=Docker Application Container Engine
Documentation=https://docs.docker.com
After=network-online.target containerd.service
Wants=network-online.target
Requires=containerd.service

[Service]
ExecStart=/usr/local/bin/dockerd
Restart=always
RestartSec=5
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
OOMScoreAdjust=-500

[Install]
WantedBy=multi-user.target
`
	if err := writeRemote(root, "/etc/systemd/system/containerd.service", containerdUnit); err != nil {
		return err
	}
	return writeRemote(root, "/etc/systemd/system/docker.service", dockerUnit)
}

func writeDaemonJSON(root *privilege.Runner, mirror string, liveRestore bool, ui UI) error {
	// 已有配置先备份（防覆盖用户自定义项）
	if root.QuietRun("test", "-f", "/etc/docker/daemon.json") == nil {
		bak := "/etc/docker/daemon.json.bak-" + timeNowStamp()
		if err := root.Run("cp", "-a", "/etc/docker/daemon.json", bak); err == nil {
			ui.Warn("检测到已有 /etc/docker/daemon.json，原配置已备份为 %s（registry-mirrors 之外的字段会被本次配置覆盖，如有自定义请自行合并）", bak)
		}
	}
	mirrors := []string{}
	for _, m := range strings.Split(mirror, ",") {
		if m = strings.TrimSpace(m); m != "" {
			mirrors = append(mirrors, m)
		}
	}
	fields := []string{`"registry-mirrors": [` + joinQuoted(mirrors) + `]`}
	if liveRestore {
		fields = append(fields, `"live-restore": true`)
	}
	fields = append(fields, `"log-driver": "json-file"`, `"log-opts": {"max-size": "50m", "max-file": "3"}`)
	json := "{\n  " + strings.Join(fields, ",\n  ") + "\n}"
	return writeRemote(root, "/etc/docker/daemon.json", json)
}

func joinQuoted(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = `"` + s + `"`
	}
	return strings.Join(q, ", ")
}

// writeRemote 以 root 写文件：先落本地临时文件再 root install（避免 sudo -S 密码与内容共用 stdin）。
func writeRemote(root *privilege.Runner, path, content string) error {
	tmp, err := os.CreateTemp("", "dok-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	root.QuietRun("mkdir", "-p", parent(path))
	if err := root.Run("install", "-m", "0644", tmpName, path); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

func parent(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// extractPayload 解出 embed 的 payload（tar.gz 格式单文件 payload.bin；兼容目录形式）。
func extractPayload(dst string) error {
	// pack 注入的是 payload/payload.bin (tar.gz)；开发目录形式直接 walk
	if f, err := payloadFS.Open("payload/payload.bin"); err == nil {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			target := filepath.Join(dst, hdr.Name)
			if hdr.Typeflag == tar.TypeDir {
				os.MkdirAll(target, 0755)
				continue
			}
			os.MkdirAll(filepath.Dir(target), 0755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	// 目录形式（开发构建）
	return extractDirFS(payloadFS, "payload", dst)
}

func extractDirFS(fsys embed.FS, root, dst string) error {
	return walkFS(fsys, root, func(name string, r io.Reader, mode os.FileMode) error {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		os.MkdirAll(filepath.Dir(target), 0755)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, r)
		return err
	})
}

func walkFS(fsys embed.FS, dir string, fn func(name string, r io.Reader, mode os.FileMode) error) error {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if err := walkFS(fsys, p, fn); err != nil {
				return err
			}
			continue
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0755)
		if !strings.Contains(e.Name(), "compose") && e.Name() != "docker" && e.Name() != "dockerd" {
			mode = 0644
		}
		err = fn(p, f, mode)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func dirExists(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// DebugExtract 导出解包逻辑供调试（debug-extract 子命令用）。
func DebugExtract(dst string) error { return extractPayload(dst) }

// waitDaemon 轮询 docker info 直到就绪或超时。
func waitDaemon(root *privilege.Runner, timeoutSec int) error {
	for i := 0; i < timeoutSec; i++ {
		if root.QuietRun("/usr/local/bin/docker", "info") == nil {
			return nil
		}
		sleep(root, 1)
	}
	out, _ := root.CombinedOutput("/usr/local/bin/docker", "info")
	return fmt.Errorf("dockerd %d 秒内未就绪；检查日志: journalctl -u docker 或 /var/log/dockerd.log\n%s", timeoutSec, out)
}
