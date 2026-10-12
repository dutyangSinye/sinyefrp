package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS 内嵌前端构建产物。
//
// 目录必须至少存在一个文件，否则 go:embed 编译失败；因此在尚未执行
// 前端构建时由 internal/web/dist/index.html 占位，构建脚本会覆盖它。
//
//go:embed all:dist
var distFS embed.FS

// staticFS 是去掉 dist 前缀后的文件系统视图。
var staticFS = mustSubFS()

func mustSubFS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// 只在 embed 声明与实际目录不一致时发生，属于构建期问题。
		panic("web: 无法读取内嵌前端资源: " + err.Error())
	}
	return sub
}

// handleStatic 提供前端静态资源，并对未知路径回退到 index.html。
//
// SPA 使用前端路由（/clients、/settings 等），直接刷新这些地址时
// 服务端并不存在对应文件，必须回退到入口页，否则会 404。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	upath := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(upath, "/")
	if rel == "" {
		rel = "index.html"
	}

	if f, err := staticFS.Open(rel); err == nil {
		_ = f.Close()
		// 静态资源带哈希指纹的通常可长缓存；这里统一交给 http.ServeContent
		// 处理条件请求，只对 index.html 禁用缓存，避免升级后拿到旧入口。
		if rel == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
		return
	}

	// 未匹配到文件：回退入口页（SPA 路由）。
	serveIndex(w)
}

// serveIndex 输出内嵌的 index.html。
func serveIndex(w http.ResponseWriter) {
	data, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		http.Error(w, "前端资源缺失，请先执行前端构建", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
