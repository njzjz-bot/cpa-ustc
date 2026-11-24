package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cpa-ustc/internal/ustc"
)

const iwanRepository = "yyy1mu/ustc-iwan"

// InstallIwan requires explicit invocation and verifies GitHub's SHA-256 asset digest.
// No root, shell scripts, system VPN changes or fallback to unsigned assets are used.
func InstallIwan(ctx context.Context, stateDir string) (string, error) {
	architecture := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	platform := map[string]string{"linux": "linux", "darwin": "macos", "windows": "windows"}[runtime.GOOS]
	if architecture == "" || platform == "" {
		return "", errors.New("此平台没有受支持的 iWAN 预编译客户端，请自行安装")
	}
	assetName := "iwan-client-oidc-" + platform + "-" + architecture
	if platform == "linux" {
		assetName += "-musl"
	}
	assetName += ".zip"
	return installIwan(ctx, stateDir, platform, assetName, &http.Client{Timeout: 2 * time.Minute})
}

func installIwan(ctx context.Context, stateDir, platform, assetName string, client *http.Client) (string, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+iwanRepository+"/releases/latest", nil)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "cpa-ustc/"+ustc.Version)
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("无法读取 iWAN 发布信息，请手动安装或使用官方客户端")
	}
	defer response.Body.Close()
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release) != nil || release.Tag == "" || release.Draft || release.Prerelease {
		return "", errors.New("无法读取 iWAN 最新稳定版发布信息（可能触发 GitHub 频率限制）")
	}
	downloadURL := "https://github.com/" + iwanRepository + "/releases/download/" + url.PathEscape(release.Tag) + "/" + assetName
	for _, asset := range release.Assets {
		if asset.Name != assetName {
			continue
		}
		if !strings.HasPrefix(asset.Digest, "sha256:") || len(asset.Digest) != 71 || asset.URL != downloadURL {
			return "", errors.New("iWAN 附件缺少可信 SHA-256 摘要或来源不匹配，拒绝安装")
		}
		request, _ = http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		archive, downloadErr := client.Do(request)
		if downloadErr != nil {
			return "", errors.New("iWAN 附件下载失败")
		}
		data, readErr := io.ReadAll(io.LimitReader(archive.Body, 32<<20+1))
		archive.Body.Close()
		if readErr != nil || archive.StatusCode != 200 || len(data) > 32<<20 {
			return "", errors.New("iWAN 附件下载失败或大小异常")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != strings.TrimPrefix(asset.Digest, "sha256:") {
			return "", errors.New("iWAN 附件 SHA-256 校验失败，拒绝安装")
		}
		bundle, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if zipErr != nil {
			return "", zipErr
		}
		for _, entry := range bundle.File {
			base := filepath.Base(entry.Name)
			if entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 || (!strings.HasPrefix(base, "iwan-client-oidc-") && base != "iwan-client-oidc" && base != "iwan-client-oidc.exe") {
				continue
			}
			if entry.UncompressedSize64 > 64<<20 {
				return "", errors.New("iWAN 可执行文件大小异常")
			}
			file, openErr := entry.Open()
			if openErr != nil {
				return "", openErr
			}
			binary, readErr := io.ReadAll(io.LimitReader(file, 64<<20+1))
			file.Close()
			if readErr != nil || len(binary) == 0 || len(binary) > 64<<20 {
				return "", errors.New("iWAN 可执行文件解压失败")
			}
			name := "iwan-client-oidc"
			if platform == "windows" {
				name += ".exe"
			}
			path := filepath.Join(stateDir, "bin", name)
			if err = ustc.WritePrivate(path, binary); err != nil {
				return "", err
			}
			if err = os.Chmod(path, 0700); err != nil {
				return "", err
			}
			return path, nil
		}
		return "", errors.New("iWAN 附件中没有预期的可执行文件")
	}
	return "", errors.New("此平台没有对应的 iWAN 发布附件，请手动安装")
}
