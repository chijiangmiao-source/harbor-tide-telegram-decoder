package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// 批量请求体上限：100 条 21 字节报文加 JSON 包装后不足 4KB，
// 32KB 已足够宽松，与单条接口一样以体量上限兜底。
const maxBatchBodyBytes = 32 << 10

// decodeBatchRequest 是 POST /decode-batch 的请求模型。
type decodeBatchRequest struct {
	Year      int      `json:"year"`
	Telegrams []string `json:"telegrams"`
}

// batchResult 是 results 数组的单项。
// 成功时携带与 /decode 相同的四个业务字段（level_mm 用指针，使 0 值也会
// 出现在 JSON 中）；失败时四个业务字段整体省略，只保留 index 与既有错误码。
type batchResult struct {
	Index      int    `json:"index"`
	Station    string `json:"station,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	LevelMM    *int   `json:"level_mm,omitempty"`
	Trend      string `json:"trend,omitempty"`
	Error      string `json:"error,omitempty"`
}

// decodeBatchResponse 与输入等长、顺序一致。
type decodeBatchResponse struct {
	Results []batchResult `json:"results"`
}

func decodeBatchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 形态一：Content-Type 必须为 application/json；charset 缺省或 UTF-8 均可。
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w)
		return
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		writeError(w)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBatchBodyBytes))
	if err != nil || len(body) == 0 {
		writeError(w)
		return
	}

	// 形态二：JSON 结构必须合法——恰好一个对象、无重复键、无未知字段、
	// 顶层无第二个 JSON 值；year 为整数、telegrams 为字符串数组均由
	// 强类型解码保证（数字带小数、类型不符等都会在此被拒）。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req decodeBatchRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w)
		return
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF || hasDuplicateKey(body) {
		writeError(w)
		return
	}

	// 形态三：year 必须是 2000-2099；telegrams 为 1-100 条。
	// 单条报文长度/内容不在此校验——它属于逐条解码的 FORMAT_ERROR。
	if req.Year < 2000 || req.Year > 2099 ||
		len(req.Telegrams) < 1 || len(req.Telegrams) > 100 {
		writeError(w)
		return
	}

	// 逐条直接调用现有解码函数；单条失败不阻断其余结果，判定顺序与
	// /decode 完全一致（FORMAT -> CHECKSUM -> RANGE），且失败项不回显原报文。
	results := make([]batchResult, len(req.Telegrams))
	for i, tg := range req.Telegrams {
		t, code := decodeTelegram(tg, req.Year)
		if code != "" {
			results[i] = batchResult{Index: i, Error: code}
			continue
		}
		s := newSuccessResponse(t)
		results[i] = batchResult{
			Index:      i,
			Station:    s.Station,
			ObservedAt: s.ObservedAt,
			LevelMM:    &s.LevelMM,
			Trend:      s.Trend,
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(decodeBatchResponse{Results: results})
}

// hasDuplicateKey 报告 JSON 文档中同一对象内是否出现重复键（RFC 8259
// 建议键唯一）。入参是语法已合法的 JSON。
func hasDuplicateKey(b []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() bool
	walk = func() bool {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return false
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return false
				}
				key := keyTok.(string)
				if seen[key] {
					return true
				}
				seen[key] = true
				if walk() {
					return true
				}
			}
			_, _ = dec.Token() // 消费 '}'
		case '[':
			for dec.More() {
				if walk() {
					return true
				}
			}
			_, _ = dec.Token() // 消费 ']'
		}
		return false
	}
	return walk()
}
