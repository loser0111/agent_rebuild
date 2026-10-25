package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// spaHandler 托管前端静态产物（web/dist），支持 SPA 路由回退。
//
// 行为：
//   - 命中真实文件 → 直接返回（带缓存头）；
//   - 未命中且请求不像静态资源 → 回退到 index.html（前端路由接管）；
//   - 未命中且像静态资源（有扩展名）→ 404，避免把 JS/CSS 404 伪装成 HTML。
//
// 目录不存在时返回 503 并给出构建提示，而不是静默 404，
// 便于单端口部署时快速发现「忘了 npm run build」。
type spaHandler struct {
	root      string
	fileServe http.Handler
}

// newSPAHandler 创建静态托管 handler。root 为 dist 目录路径。
func newSPAHandler(root string) http.Handler {
	return &spaHandler{
		root:      root,
		fileServe: http.FileServer(http.Dir(root)),
	}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 只允许读操作。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if info, err := os.Stat(h.root); err != nil || !info.IsDir() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(
			"前端产物不存在：请先执行 `make build-web`（或 cd web && npm run build），" +
				"或改用 `make dev` 以开发模式运行。\n",
		))
		return
	}

	// 清理路径，禁止越界访问（../）。
	clean := path.Clean("/" + r.URL.Path)
	fsPath := filepath.Join(h.root, filepath.FromSlash(clean))

	if info, err := os.Stat(fsPath); err == nil && !info.IsDir() {
		// 带 hash 的构建产物可长缓存；index.html 必须禁缓存以便发版即时生效。
		if strings.Contains(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.fileServe.ServeHTTP(w, r)
		return
	}

	// 目录或不存在：含扩展名视为静态资源缺失，返回 404。
	if ext := path.Ext(clean); ext != "" && ext != ".html" {
		http.NotFound(w, r)
		return
	}

	// SPA 回退：返回 index.html。
	index := filepath.Join(h.root, "index.html")
	if _, err := os.Stat(index); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "读取前端入口失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}
