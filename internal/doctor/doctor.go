// Package doctor 实现安装前环境预检（红黄绿报告）。
package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"docker-offline-kit/internal/arch"
)

type Status int

const (
	OK Status = iota
	WARN
	FAIL
)

func (s Status) Icon() string {
	switch s {
	case OK:
		return "✓"
	case WARN:
		return "⚠"
	default:
		return "✗"
	}
}

type Check struct {
	Name   string
	Status Status
	Detail string
	// Fix 是红灯/黄灯时的修复建议
	Fix string
}

type Report struct {
	Checks []Check
	Fails  int
	Warns  int
}

func (r *Report) Verdict() string {
	switch {
	case r.Fails > 0:
		return fmt.Sprintf("存在 %d 个阻断项，无法安装（请先按 ✗ 项处理）", r.Fails)
	case r.Warns > 0:
		return fmt.Sprintf("可以安装，注意 %d 个 ⚠ 提示", r.Warns)
	default:
		return "环境完全兼容，可直接安装"
	}
}

// Check1Identity 身份与提权路径（供 privilege 模块填充 detail）。
func identity() Check {
	if os.Geteuid() == 0 {
		return Check{Name: "身份", Status: OK, Detail: "root 用户，直接安装"}
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		if exec.Command("sudo", "-n", "true").Run() == nil {
			return Check{Name: "身份", Status: OK, Detail: "普通用户，sudo 免密可用"}
		}
		return Check{Name: "身份", Status: WARN, Detail: "普通用户，sudo 需要密码（安装时会询问）"}
	}
	if _, err := exec.LookPath("su"); err == nil {
		return Check{Name: "身份", Status: WARN, Detail: "无 sudo，将回退 su（需要 root 密码）"}
	}
	return Check{Name: "身份", Status: FAIL, Detail: "非 root 且无 sudo/su", Fix: "请以 root 登录，或让管理员配置 sudo"}
}

// Run 执行全部预检。kernelCheck 等需要 root 的探测在非 root 下自动降级。
func Run() *Report {
	r := &Report{}

	// 1) 身份
	r.add(identity())

	// 2) 架构
	da, err := arch.CurrentDockerArch()
	if err != nil {
		r.add(Check{Name: "架构", Status: FAIL, Detail: err.Error()})
	} else {
		r.add(Check{Name: "架构", Status: OK, Detail: fmt.Sprintf("%s（安装器含此架构）", da)})
	}

	// 3) 内核 >= 3.10
	kv := kernelVersion()
	if kv >= 3.10 {
		r.add(Check{Name: "内核", Status: OK, Detail: release() + " (≥3.10)"})
	} else {
		r.add(Check{Name: "内核", Status: FAIL, Detail: release() + " 过旧(<3.10)", Fix: "升级内核后重试"})
	}

	// 4) overlay2（非 root 降级为读 /proc/filesystems）
	if os.Geteuid() == 0 {
		if err := exec.Command("modprobe", "overlay").Run(); err == nil || fsHasOverlay() {
			r.add(Check{Name: "存储驱动", Status: OK, Detail: "overlay2 可用"})
		} else {
			r.add(Check{Name: "存储驱动", Status: FAIL, Detail: "overlay 内核模块不可用", Fix: "升级内核，或接受降级 vfs（性能差）"})
		}
	} else if fsHasOverlay() {
		r.add(Check{Name: "存储驱动", Status: OK, Detail: "overlay2 可用（/proc/filesystems）"})
	} else {
		r.add(Check{Name: "存储驱动", Status: FAIL, Detail: "/proc/filesystems 无 overlay", Fix: "需 root 下 modprobe overlay 确认；内核可能过旧"})
	}

	// 5) cgroup 版本
	if fileExists("/sys/fs/cgroup/cgroup.controllers") {
		r.add(Check{Name: "cgroup", Status: OK, Detail: "v2"})
	} else {
		r.add(Check{Name: "cgroup", Status: OK, Detail: "v1"})
	}

	// 6) iptables（官方前置：>=1.4；缺失是最常见的"容器没网"根因）
	// 注意：非 root 用户 PATH 常不含 /usr/sbin，需补查（否则误报）
	if p, err := lookBin("iptables"); err == nil {
		r.add(Check{Name: "iptables", Status: OK, Detail: p})
	} else if _, err := lookBin("nft"); err == nil {
		r.add(Check{Name: "iptables", Status: WARN, Detail: "无 iptables 但有 nft；多数发行版 iptables 为 nft 后端别名，若容器无网需补装 iptables"})
	} else {
		r.add(Check{Name: "iptables", Status: FAIL, Detail: "系统缺 iptables，容器网络/端口映射将不可用", Fix: "离线安装发行版 iptables 包（如 rpm/deb）后重试"})
	}

	// 7) xz utils（官方前置 >=4.9）
	if _, err := lookBin("xz"); err == nil {
		r.add(Check{Name: "xz", Status: OK, Detail: "可用"})
	} else {
		r.add(Check{Name: "xz", Status: WARN, Detail: "无 xz；仅影响部分镜像层解压，基础功能不受影响", Fix: "建议离线安装 xz-utils"})
	}

	// 8) systemd
	if dirExists("/run/systemd/system") || pidof("systemd") {
		r.add(Check{Name: "systemd", Status: OK, Detail: "可用，将注册开机自启服务"})
	} else {
		r.add(Check{Name: "systemd", Status: WARN, Detail: "无 systemd，将走 nohup 兜底（无开机自启）"})
	}

	// 9) 旧版 docker（区分安装来源：包管理器安装的覆盖会造成双版本共存，需明确警告）
	if p, err := exec.LookPath("docker"); err == nil {
		ver := cmdOut(p, "--version")
		if src := pkgSource(); src != "" {
			r.add(Check{Name: "旧版 Docker", Status: WARN,
				Detail: ver + "，来源: " + src + "（包管理器管理）",
				Fix:    "建议继续用包管理器升级；用本工具覆盖会双版本共存（apt upgrade 时 /usr/bin/docker 被独立升级，与 /usr/local/bin 版本漂移）"})
		} else {
			r.add(Check{Name: "旧版 Docker", Status: WARN, Detail: ver + "（静态/手动安装），将覆盖升级（/var/lib/docker 数据保留）"})
		}
	} else {
		r.add(Check{Name: "旧版 Docker", Status: OK, Detail: "无，全新安装"})
	}

	// 9.5) rootless 可行性（仅当无特权路径时评估）
	if os.Geteuid() != 0 {
		if _, err := exec.LookPath("sudo"); err != nil {
			if _, serr := exec.LookPath("su"); serr != nil {
				detail, st := RootlessPrereqs()
				r.add(Check{Name: "rootless", Status: st, Detail: detail,
					Fix: "管理员一次性: 安装 uidmap + usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <用户> + sysctl kernel.apparmor_restrict_unprivileged_userns=0"})
			}
		}
	}

	// 10) 磁盘
	if avail := diskAvailMB("/var/lib"); avail > 0 {
		if avail >= 2048 {
			r.add(Check{Name: "磁盘", Status: OK, Detail: fmt.Sprintf("/var/lib 可用 %d MB", avail)})
		} else {
			r.add(Check{Name: "磁盘", Status: WARN, Detail: fmt.Sprintf("/var/lib 可用仅 %d MB（<2GB），拉镜像可能失败", avail)})
		}
	}

	return r
}

func (r *Report) add(c Check) {
	r.Checks = append(r.Checks, c)
	switch c.Status {
	case WARN:
		r.Warns++
	case FAIL:
		r.Fails++
	}
}

// ---------- 探测小工具 ----------

// lookBin 在 PATH + 常见 sbin 目录中查找可执行文件（非 root PATH 可能缺 sbin）。
func lookBin(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, d := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

func fileExists(p string) bool  { _, err := os.Stat(p); return err == nil }
func dirExists(p string) bool   { st, err := os.Stat(p); return err == nil && st.IsDir() }

func fsHasOverlay() bool {
	b, err := os.ReadFile("/proc/filesystems")
	return err == nil && strings.Contains(string(b), "overlay")
}

func pidof(name string) bool {
	return exec.Command("pidof", name).Run() == nil
}

func release() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "未知内核"
	}
	return strings.TrimSpace(string(b))
}

func kernelVersion() float64 {
	parts := strings.SplitN(release(), ".", 3)
	if len(parts) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(parts[0]+"."+parts[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// pkgSource 检测 docker 是否由包管理器管理（dpkg/rpm），返回来源描述；无则空串。
func pkgSource() string {
	if _, err := exec.LookPath("dpkg"); err == nil {
		script := `dpkg -l 2>/dev/null | awk '/^ii/ && $2 ~ /^(docker-ce|docker-ee|docker.io|containerd|nvidia-docker2)$/ {print $2, $3}'`
		if out := cmdOut("sh", "-c", script); strings.TrimSpace(out) != "" {
			return "dpkg: " + strings.ReplaceAll(out, "\n", "; ")
		}
	}
	if _, err := exec.LookPath("rpm"); err == nil {
		script := `rpm -q docker-ce docker-ee docker.io containerd 2>/dev/null | grep -v "not installed" | grep -v 未安装`
		if out := cmdOut("sh", "-c", script); strings.TrimSpace(out) != "" {
			return "rpm: " + strings.ReplaceAll(out, "\n", "; ")
		}
	}
	return ""
}

// RootlessPrereqs 评估无特权机器走 rootless 的可行性。
func RootlessPrereqs() (string, Status) {
	issues := []string{}
	if n := usernsMax(); n <= 0 {
		issues = append(issues, "user namespaces 未启用")
	}
	for _, b := range []string{"newuidmap", "newgidmap"} {
		if _, err := lookBin(b); err != nil {
			issues = append(issues, "缺 uidmap 包")
			break
		}
	}
	// Ubuntu 23.10+ 的 AppArmor 非特权 userns 限制
	if b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(b)) == "1" {
		issues = append(issues, "AppArmor 限制了非特权 userns（Ubuntu 24.04+ 默认）")
	}
	u, uerr := user.Current()
	hasSub := false
	if uerr == nil {
		for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
			b, ferr := os.ReadFile(f)
			if ferr != nil || !strings.Contains(string(b), u.Username+":") {
				break
			}
			hasSub = f == "/etc/subgid"
		}
	}
	if !hasSub {
		issues = append(issues, "缺 /etc/subuid、/etc/subgid 条目")
	}
	if len(issues) == 0 {
		return "rootless 前提全部满足（无特权机器可装）", OK
	}
	needAdmin := false
	for _, i := range issues {
		if strings.Contains(i, "uidmap") || strings.Contains(i, "subuid") || strings.Contains(i, "AppArmor") {
			needAdmin = true
		}
	}
	detail := "rootless 前提缺失: " + strings.Join(issues, "；")
	if needAdmin {
		return detail, WARN // 管理员一次性配置后可行
	}
	return detail, FAIL
}

func cmdOut(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return name
	}
	return strings.TrimSpace(string(out))
}

func usernsMax() int {
	b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func diskAvailMB(path string) int64 {
	// 用 df -BM --output=avail，避免 syscall statfs 的平台差异
	out, err := exec.Command("df", "-BM", "--output=avail", path).CombinedOutput()
	if err != nil {
		return -1
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) < 2 {
		return -1
	}
	v := strings.TrimSuffix(lines[len(lines)-1], "M")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
