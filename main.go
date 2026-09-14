package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

// maxBodyBytes 限制请求体大小；合法报文只有 21 字节。
const maxBodyBytes = 1 << 10

// successResponse 是成功时唯一允许返回的四个字段。
type successResponse struct {
	Station    string `json:"station"`
	ObservedAt string `json:"observed_at"`
	LevelMM    int    `json:"level_mm"`
	Trend      string `json:"trend"`
}

// newSuccessResponse 由领域对象 telegram 构造响应模型，
// 供 /decode 与 /decode-batch 共享。
func newSuccessResponse(t telegram) successResponse {
	return successResponse{
		Station:    t.Station,
		ObservedAt: t.ObservedAt.Format("2006-01-02T15:04:05Z"),
		LevelMM:    t.LevelMM,
		Trend:      t.Trend,
	}
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "对本地 API 做一次健康检查后退出（供容器 HEALTHCHECK 使用）")
	flag.Parse()
	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	mux := newMux()

	addr := ":" + port()
	log.Printf("tidegram listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// runHealthcheck 在容器内部探测 /healthz，供 Dockerfile HEALTHCHECK 复用同一二进制。
func runHealthcheck() int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port() + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func decodeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 形态一：Content-Type 必须为 text/plain；charset 缺省或 UTF-8 均可。
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "text/plain" {
		writeError(w)
		return
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		writeError(w)
		return
	}

	// 形态二：X-Observation-Year 必须是 2000-2099 的四位十进制年份。
	yearRaw := r.Header.Get("X-Observation-Year")
	if len(yearRaw) != 4 || !allDigits(yearRaw) {
		writeError(w)
		return
	}
	yearInt := int(yearRaw[0]-'0')*1000 + int(yearRaw[1]-'0')*100 +
		int(yearRaw[2]-'0')*10 + int(yearRaw[3]-'0')
	if yearInt < 2000 || yearInt > 2099 {
		writeError(w)
		return
	}

	// 形态三：单行紧凑报文。控制字符（含 CR/LF）与非 ASCII 字节都会在
	// decodeTelegram 中被拒绝，因此这里只需原样读出请求体。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil || len(body) == 0 {
		writeError(w)
		return
	}

	t, code := decodeTelegram(string(body), yearInt)
	if code != "" {
		writeError(w, code)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(newSuccessResponse(t))
}

// newMux 注册全部路由；测试与 main 共用同一份路由表。
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/decode", decodeHandler)
	mux.HandleFunc("/decode-batch", decodeBatchHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// writeError 只输出错误码本身，不携带任何报文字段信息。
func writeError(w http.ResponseWriter, code ...string) {
	c := statusFormat
	if len(code) == 1 {
		c = code[0]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, c)
}
