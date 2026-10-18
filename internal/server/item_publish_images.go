package server

// item_publish_images.go: image zip extraction, image loading/validation, and safe path helpers for publish batches.

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func extractPublishImagesZip(raw []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("解析图片 zip 失败: %w", err)
	}
	extractedFiles := 0
	extractedBytes := int64(0)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		extractedFiles++
		if extractedFiles > maxItemPublishZipFiles {
			return fmt.Errorf("图片 zip 文件数不能超过 %d", maxItemPublishZipFiles)
		}
		rel, err := safeZipPath(f.Name)
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(dest)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
			_ = root.Close()
			return err
		}
		rc, err := f.Open()
		if err != nil {
			_ = root.Close()
			return err
		}
		data, err := io.ReadAll(io.LimitReader(rc, (10<<20)+1))
		_ = rc.Close()
		if err != nil {
			_ = root.Close()
			return err
		}
		if len(data) > 10<<20 {
			_ = root.Close()
			return fmt.Errorf("图片文件不能超过 10 MiB: %s", f.Name)
		}
		if len(data) == 0 {
			_ = root.Close()
			continue
		}
		if !strings.HasPrefix(http.DetectContentType(data), "image/") {
			_ = root.Close()
			continue
		}
		extractedBytes += int64(len(data))
		if extractedBytes > maxItemPublishZipExtractBytes {
			_ = root.Close()
			return fmt.Errorf("图片 zip 解压后总大小不能超过 %d MiB", maxItemPublishZipExtractBytes>>20)
		}
		file, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = file.Write(data)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
		_ = root.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFileWithinRoot 将上传文件限制在指定根目录内，拒绝符号链接和路径逃逸。
func writeFileWithinRoot(rootDir, name string, data []byte) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func safeBaseName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	if name == "." || name == "/" {
		return ""
	}
	return name
}

func safeZipPath(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("图片路径不安全: %s", raw)
	}
	clean := filepath.Clean(raw)
	if clean == "." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("图片路径不安全: %s", raw)
	}
	return clean, nil
}
