// Package arch 提供 CPU 架构识别与映射。
package arch

import (
	"fmt"
	"runtime"
)

// DockerArch 将 uname -m 输出映射为 docker 静态包目录名。
// 支持 x86_64 / aarch64 / armv7(armhf)。
func DockerArch(unameM string) (string, error) {
	switch unameM {
	case "x86_64", "amd64":
		return "x86_64", nil
	case "aarch64", "arm64":
		return "aarch64", nil
	case "armv7l", "armv6l", "arm":
		return "armhf", nil // 32 位 ARM（download.docker.com 目录名 armhf）
	default:
		return "", fmt.Errorf("不支持的架构: %s（支持 x86_64/aarch64/armv7）", unameM)
	}
}

// CurrentDockerArch 返回当前机器的 docker 架构目录名。
func CurrentDockerArch() (string, error) {
	return DockerArch(unameM())
}

// GoArch 返回 docker 架构名对应的 GOARCH/GOARM 构建参数。
func GoArch(dockerArch string) (goarch, goarm string) {
	switch dockerArch {
	case "x86_64":
		return "amd64", ""
	case "aarch64":
		return "arm64", ""
	case "armhf":
		return "arm", "7"
	}
	return runtime.GOARCH, ""
}

// AllArchs 是 pack 支持的全部架构。
var AllArchs = []string{"x86_64", "aarch64", "armhf"}

// ComposeAssetSuffix 返回 compose release 资产名中的架构后缀。
func ComposeAssetSuffix(dockerArch string) string {
	switch dockerArch {
	case "x86_64":
		return "x86_64"
	case "aarch64":
		return "aarch64"
	case "armhf":
		return "armv7"
	}
	return dockerArch
}
