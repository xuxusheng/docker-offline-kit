// pack 在联网机上制作离线安装器：下载官方静态二进制 → 注入 payload →
// 交叉编译各架构 Go 安装器 → 拼接 universal bash 引导壳 → 产出 sha256。
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"docker-offline-kit/internal/arch"
)

var (
	dockerVersion  string
	composeVersion string
	archs          []string
	outDir         string
)

func main() {
	root := &cobra.Command{
		Use:   "pack",
		Short: "制作离线安装器（联网机运行）",
		RunE:  run,
	}
	root.Flags().StringVar(&dockerVersion, "docker-version", "29.7.2", "Docker Engine 版本")
	root.Flags().StringVar(&composeVersion, "compose-version", "v5.5.0", "Compose 版本")
	root.Flags().StringSliceVar(&archs, "archs", []string{"x86_64", "aarch64"}, "架构列表（可含 arm）")
	root.Flags().StringVar(&outDir, "out", "release", "输出目录")
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	os.MkdirAll(outDir, 0755)
	workDir, err := os.MkdirTemp("", "dok-pack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	// 1) 下载各架构 payload
	var payloads []string // dist 内各架构安装器路径
	for _, a := range archs {
		if _, err := arch.DockerArch(map[string]string{"x86_64": "x86_64", "aarch64": "aarch64", "armhf": "armv7l"}[a]); err != nil {
			return fmt.Errorf("未知架构 %s（可选: %v）", a, arch.AllArchs)
		}
		fmt.Printf("==> [%s] 下载 Docker %s\n", a, dockerVersion)
		engineDir := filepath.Join(workDir, "engine-"+a)
		if err := downloadExtractTgz(
			fmt.Sprintf("https://download.docker.com/linux/static/stable/%s/docker-%s.tgz", a, dockerVersion),
			engineDir); err != nil {
			return fmt.Errorf("[%s] 下载引擎失败: %w", a, err)
		}
		fmt.Printf("==> [%s] 下载 Compose %s\n", a, composeVersion)
		composePath := filepath.Join(workDir, "compose-"+a)
		if err := download(
			fmt.Sprintf("https://github.com/docker/compose/releases/download/%s/docker-compose-linux-%s", composeVersion, arch.ComposeAssetSuffix(a)),
			composePath); err != nil {
			return fmt.Errorf("[%s] 下载 compose 失败: %w", a, err)
		}

		// 2) 组 payload.bin（tar.gz: bin/* + compose/docker-compose）
		fmt.Printf("==> [%s] 组装 payload\n", a)
		payloadBin := filepath.Join(workDir, "payload-"+a+".bin")
		if err := buildPayload(payloadBin, engineDir, composePath); err != nil {
			return err
		}

		// 3) 注入并交叉编译安装器
		fmt.Printf("==> [%s] 编译安装器\n", a)
		instPath, err := buildInstaller(a, payloadBin)
		if err != nil {
			return err
		}
		payloads = append(payloads, instPath)
	}

	// 4) universal .run
	name := fmt.Sprintf("docker-offline-installer-%s+%s-universal.run",
		dockerVersion, strings.TrimPrefix(composeVersion, "v"))
	runPath := filepath.Join(outDir, name)
	fmt.Printf("==> 拼接 universal 引导壳 -> %s\n", runPath)
	if err := buildUniversal(runPath, payloads, archs); err != nil {
		return err
	}

	// 5) sha256
	sum, err := sha256File(runPath)
	if err != nil {
		return err
	}
	os.WriteFile(runPath+".sha256", []byte(sum+"  "+filepath.Base(runPath)+"\n"), 0644)

	fmt.Printf("\n✓ 完成: %s (%d MB)\n  sha256: %s\n", runPath, fileSizeMB(runPath), sum[:16]+"...")
	fmt.Printf("  使用: 拷到目标机后  sudo bash %s\n", filepath.Base(runPath))
	return nil
}

// buildInstaller 把 payload.bin 写进 embed 目录后交叉编译（串行执行，embed 编译期生效）。
func buildInstaller(dockerArch, payloadBin string) (string, error) {
	// 仅本地架构可复用当前 go build；交叉编译直接 go build 即可（纯 Go）
	embedDir := "internal/installer/payload"
	os.WriteFile(filepath.Join(embedDir, "payload.bin"), mustRead(payloadBin), 0644)
	defer func() {
		os.Remove(filepath.Join(embedDir, "payload.bin"))
		os.WriteFile(filepath.Join(embedDir, ".keep"), []byte("placeholder - replaced by pack\n"), 0644)
	}()

	goarch, goarm := arch.GoArch(dockerArch)
	out := filepath.Join("dist", fmt.Sprintf("installer-%s", dockerArch))
	os.MkdirAll("dist", 0755)
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", fmt.Sprintf("-s -w -X main.version=%s+%s", dockerVersion, strings.TrimPrefix(composeVersion, "v")),
		"-o", out, "./cmd/installer")
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0", "GOARCH="+goarch,
		func() string { if goarm != "" { return "GOARM=" + goarm }; return "GOARM=" }(),
	)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("编译 %s: %w\n%s", dockerArch, err, outb)
	}
	return out, nil
}

// buildPayload 生成 payload.bin = tar.gz{ bin/<引擎>, compose/docker-compose }。
func buildPayload(outPath, engineDir, composePath string) error {
	f, _ := os.Create(outPath)
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	add := func(name, path string, mode int64) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(data))}
		tw.WriteHeader(hdr)
		_, err = tw.Write(data)
		return err
	}
	// 递归收集引擎二进制（tgz 顶层目录名随版本变化：docker-<ver>/ 或 docker/）
	err := filepath.Walk(engineDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(engineDir, path)
		return add("bin/"+filepath.Base(rel), path, 0755)
	})
	if err != nil {
		return err
	}
	if err := add("compose/docker-compose", composePath, 0755); err != nil {
		return err
	}
	tw.Close()
	gz.Close()
	return nil
}

// buildUniversal 把各架构安装器打 tar.gz 拼到引导壳后。
func buildUniversal(outPath string, installers []string, archs []string) error {
	bootstrap, err := os.ReadFile("scripts/bootstrap.sh")
	if err != nil {
		return fmt.Errorf("读取引导壳模板失败: %w（请在仓库根目录运行 pack）", err)
	}
	tarPath := outPath + ".payload.tar.gz"
	if err := writeTarOfFiles(tarPath, installers, archs); err != nil {
		return err
	}
	defer os.Remove(tarPath)

	out, _ := os.Create(outPath)
	defer out.Close()
	out.Write(bootstrap)
	if !strings.HasSuffix(string(bootstrap), "\n") {
		out.Write([]byte("\n"))
	}
	fmt.Fprintln(out, "__DOK_PAYLOAD_BELOW__")
	tarData, _ := os.ReadFile(tarPath)
	out.Write(tarData)
	os.Chmod(outPath, 0755)
	return nil
}

func writeTarOfFiles(outPath string, files, names []string) error {
	f, _ := os.Create(outPath)
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for i, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: names[i], Mode: 0755, Size: int64(len(data))}
		tw.WriteHeader(hdr)
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	return nil
}

// ---------- 通用工具 ----------

func download(url, outPath string) error {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	out, _ := os.Create(outPath)
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func downloadExtractTgz(url, dst string) error {
	tmp := dst + ".tgz"
	if err := download(url, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
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
		io.Copy(out, tr)
		out.Close()
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSizeMB(path string) int64 {
	st, _ := os.Stat(path)
	return st.Size() / 1024 / 1024
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}
