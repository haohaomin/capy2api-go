// Package panel 内嵌管理面板的静态资源。
package panel

import (
	"embed"
	"path"
	"strings"
)

//go:embed assets/*
var files embed.FS

// Asset 是一份静态资源。
type Asset struct {
	Body        []byte
	ContentType string
}

// Lookup 读取面板资源；name 为空或不存在时由调用方决定回退。
func Lookup(name string) (Asset, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || strings.HasSuffix(name, "/") {
		name = "index.html"
	}
	if strings.Contains(name, "..") {
		return Asset{}, false
	}
	raw, err := files.ReadFile(path.Join("assets", name))
	if err != nil {
		return Asset{}, false
	}
	return Asset{Body: raw, ContentType: contentType(name)}, true
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
