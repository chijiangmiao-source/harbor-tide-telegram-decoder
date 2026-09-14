package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- 请求构造辅助 ----

func batchRequestRaw(t *testing.T, raw string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/decode-batch", strings.NewReader(raw))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	decodeBatchHandler(rec, req)
	return rec
}

func batchRequest(t *testing.T, year int, telegrams []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(decodeBatchRequest{Year: year, Telegrams: telegrams})
	if err != nil {
		t.Fatal(err)
	}
	return batchRequestRaw(t, string(body), "application/json")
}

func decodeBatchRaw(t *testing.T, rec *httptest.ResponseRecorder) decodeBatchResponse {
	t.Helper()
	var got decodeBatchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return got
}

// levelMM 以 int 形式读取潮位（失败项无该字段，按 0 处理）。
func (r batchResult) levelMM() int {
	if r.LevelMM == nil {
		return 0
	}
	return *r.LevelMM
}

// ---- 成功与逐字段行为 ----

func TestBatchSuccessShape(t *testing.T) {
	ok1 := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	ok2 := build(t, "TW", "NAG", "12", "31", "23", "59", "-", "0000", "DN")
	rec := batchRequest(t, 2099, []string{ok1, ok2})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}

	// 顺序、等长、index 从 0 开始。
	got := decodeBatchRaw(t, rec)
	if len(got.Results) != 2 {
		t.Fatalf("results length = %d, want 2", len(got.Results))
	}
	if got.Results[0].Index != 0 || got.Results[1].Index != 1 {
		t.Errorf("indexes = %d,%d, want 0,1", got.Results[0].Index, got.Results[1].Index)
	}

	r0 := got.Results[0]
	if r0.Error != "" {
		t.Errorf("result 0 error = %q, want empty", r0.Error)
	}
	if r0.Station != "KHI" || r0.ObservedAt != "2099-03-15T08:30:00Z" ||
		r0.levelMM() != 1234 || r0.Trend != "UP" {
		t.Errorf("result 0 payload = %+v", r0)
	}

	// -0000 归一化为 0，且 level_mm=0 必须真实出现在 JSON 中。
	r1 := got.Results[1]
	if r1.levelMM() != 0 {
		t.Errorf("result 1 level = %d, want 0", r1.levelMM())
	}
	var rawFields []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &struct {
		Results *[]map[string]json.RawMessage `json:"results"`
	}{Results: &rawFields}); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{"index": true, "station": true, "observed_at": true,
		"level_mm": true, "trend": true}
	for i, item := range rawFields {
		if len(item) != 5 {
			t.Errorf("success result %d has %d keys, want exactly 5: %v", i, len(item), item)
		}
		for k := range item {
			if !wantKeys[k] {
				t.Errorf("success result %d has unexpected key %q", i, k)
			}
		}
		if _, ok := item["level_mm"]; !ok {
			t.Errorf("success result %d missing level_mm", i)
		}
		if _, ok := item["error"]; ok {
			t.Errorf("success result %d must not contain error", i)
		}
	}
}

// ---- 混合批次：成功 + 校验错 + 越界 + 格式错，互不阻断 ----

func TestBatchMixedResults(t *testing.T) {
	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	checksumBad := withChecksum(ok, byte('0'+(int(ok[20]-'0')+1)%10))
	rangeBad := build(t, "TW", "KHI", "03", "15", "08", "30", "-", "5001", "UP")
	formatBad := "too-short"
	empty := ""

	rec := batchRequest(t, 2026, []string{ok, checksumBad, rangeBad, formatBad, empty})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, single-item failures must not fail the batch; body=%q",
			rec.Code, rec.Body.String())
	}
	got := decodeBatchRaw(t, rec)
	if len(got.Results) != 5 {
		t.Fatalf("results length = %d, want 5", len(got.Results))
	}

	want := []struct {
		err     string
		station string
		level   int
	}{
		{"", "KHI", 1234},
		{statusChecksum, "", 0},
		{statusRange, "", 0},
		{statusFormat, "", 0},
		{statusFormat, "", 0},
	}
	for i, w := range want {
		r := got.Results[i]
		if r.Index != i {
			t.Errorf("result %d index = %d", i, r.Index)
		}
		if r.Error != w.err {
			t.Errorf("result %d error = %q, want %q", i, r.Error, w.err)
		}
		if w.err == "" {
			if r.Station != w.station || r.levelMM() != w.level || r.Trend != "UP" ||
				r.ObservedAt != "2026-03-15T08:30:00Z" {
				t.Errorf("result %d payload = %+v", i, r)
			}
		} else {
			// 失败项只含 index 和错误码，不回显原报文。
			if r.Station != "" || r.LevelMM != nil || r.Trend != "" || r.ObservedAt != "" {
				t.Errorf("result %d failure leaks business fields: %+v", i, r)
			}
		}
	}

	// 原始报文不得出现在任何失败项或整体响应中。
	for _, tg := range []string{checksumBad, rangeBad, formatBad} {
		if strings.Contains(rec.Body.String(), tg) {
			t.Errorf("response echoes rejected telegram %q: %s", tg, rec.Body.String())
		}
	}

	// 失败项 JSON 键恰好为 index/error。
	var envelope struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3, 4} {
		item := envelope.Results[i]
		if len(item) != 2 {
			t.Errorf("failure result %d has %d keys (%v), want exactly index+error",
				i, len(item), item)
		}
		if _, ok := item["index"]; !ok {
			t.Errorf("failure result %d missing index", i)
		}
		if string(item["error"]) != `"`+want[i].err+`"` {
			t.Errorf("failure result %d error raw = %s", i, item["error"])
		}
	}
}

// 三类错误的判定顺序在批量通道中与单条通道保持一致。
func TestBatchPreservesDecisionOrder(t *testing.T) {
	// 越界 + 校验位错误：先 CHECKSUM。
	bad := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "9999", "UP")
	bad = withChecksum(bad, byte('0'+(int(bad[20]-'0')+1)%10))
	// 日期不存在 + 校验位错误：先 FORMAT。
	badDate := build(t, "TW", "KHI", "02", "30", "08", "30", "+", "9999", "UP")
	badDate = withChecksum(badDate, '0')

	rec := batchRequest(t, 2026, []string{bad, badDate})
	got := decodeBatchRaw(t, rec)
	if got.Results[0].Error != statusChecksum {
		t.Errorf("range+checksum: got %q, want CHECKSUM_ERROR", got.Results[0].Error)
	}
	if got.Results[1].Error != statusFormat {
		t.Errorf("date+checksum+range: got %q, want FORMAT_ERROR", got.Results[1].Error)
	}
}

// 年份作用于批次内每条报文（闰年判定随年份变化）。
func TestBatchYearAppliesToEachItem(t *testing.T) {
	leap := build(t, "TW", "ABC", "02", "29", "09", "00", "+", "0001", "EQ")
	rec := batchRequest(t, 2023, []string{leap})
	if got := decodeBatchRaw(t, rec); got.Results[0].Error != statusFormat {
		t.Errorf("2023-02-29: got %q, want FORMAT_ERROR", got.Results[0].Error)
	}
	rec = batchRequest(t, 2024, []string{leap})
	if got := decodeBatchRaw(t, rec); got.Results[0].Error != "" || got.Results[0].Station != "ABC" {
		t.Errorf("2024-02-29: got %+v, want success", got.Results[0])
	}
}

// ---- 整批拒绝：一律 400 + 纯文本 FORMAT_ERROR ----

func TestBatchRejectedWholesale(t *testing.T) {
	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")

	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{"not json", "application/json", "TWKHI03150830+1234UP0"},
		{"empty object", "application/json", `{}`},
		{"missing telegrams", "application/json", `{"year":2026}`},
		{"missing year", "application/json", `{"telegrams":["` + ok + `"]}`},
		{"empty telegrams", "application/json", `{"year":2026,"telegrams":[]}`},
		{"year too low", "application/json", `{"year":1999,"telegrams":["` + ok + `"]}`},
		{"year too high", "application/json", `{"year":2100,"telegrams":["` + ok + `"]}`},
		{"year float", "application/json", `{"year":2026.5,"telegrams":["` + ok + `"]}`},
		{"year string", "application/json", `{"year":"2026","telegrams":["` + ok + `"]}`},
		{"year null", "application/json", `{"year":null,"telegrams":["` + ok + `"]}`},
		{"telegram not string", "application/json", `{"year":2026,"telegrams":[123]}`},
		{"unknown field", "application/json", `{"year":2026,"telegrams":["x"],"foo":1}`},
		{"duplicate year key", "application/json",
			`{"year":2026,"year":2027,"telegrams":["x"]}`},
		{"duplicate telegrams key", "application/json",
			`{"year":2026,"telegrams":["x"],"telegrams":["y"]}`},
		{"two top level values", "application/json",
			`{"year":2026,"telegrams":["x"]} {"year":2026,"telegrams":["x"]}`},
		{"array instead of object", "application/json", `["x"]`},
		{"truncated json", "application/json", `{"year":2026,"telegram`},
		{"empty body", "application/json", ``},
		{"wrong content type", "text/plain", `{"year":2026,"telegrams":["x"]}`},
		{"missing content type", "", `{"year":2026,"telegrams":["x"]}`},
		{"non utf8 charset", "application/json; charset=gbk",
			`{"year":2026,"telegrams":["x"]}`},
	}

	// 101 条合法批次。
	over := make([]string, 101)
	for i := range over {
		over[i] = ok
	}
	overBody, _ := json.Marshal(decodeBatchRequest{Year: 2026, Telegrams: over})
	cases = append(cases, struct {
		name        string
		contentType string
		body        string
	}{"over 100 items", "application/json", string(overBody)})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := batchRequestRaw(t, tc.body, tc.contentType)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != statusFormat {
				t.Errorf("body = %q, want exactly FORMAT_ERROR", rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("content-type = %q, want text/plain", ct)
			}
		})
	}
}

func TestBatchBoundaryCounts(t *testing.T) {
	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	for _, n := range []int{1, 100} {
		items := make([]string, n)
		for i := range items {
			items[i] = ok
		}
		rec := batchRequest(t, 2026, items)
		if rec.Code != http.StatusOK {
			t.Fatalf("n=%d: status = %d body=%q", n, rec.Code, rec.Body.String())
		}
		if got := decodeBatchRaw(t, rec); len(got.Results) != n {
			t.Fatalf("n=%d: got %d results", n, len(got.Results))
		}
	}
}

func TestBatchYearBoundaries(t *testing.T) {
	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	for _, y := range []int{2000, 2099} {
		rec := batchRequest(t, y, []string{ok})
		if rec.Code != http.StatusOK {
			t.Errorf("year %d: status = %d body=%q", y, rec.Code, rec.Body.String())
		}
	}
}

func TestBatchMethodNotAllowed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/decode-batch", nil)
		rec := httptest.NewRecorder()
		decodeBatchHandler(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, got)
		}
	}
}

// ---- 路由：两个端点经同一 mux 共存，互不干扰 ----

func TestMuxRoutes(t *testing.T) {
	mux := newMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// /decode 契约保持：text/plain 成功。
	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decode", strings.NewReader(ok))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Observation-Year", "2026")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/decode status = %d body=%q", resp.StatusCode, raw)
	}

	// /decode 拒绝 JSON 内容类型（不得因新端点放宽）。
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/decode",
		strings.NewReader(`{"year":2026,"telegrams":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || string(raw) != statusFormat {
		t.Fatalf("/decode with JSON: %d %q, want 400 FORMAT_ERROR", resp.StatusCode, raw)
	}

	// GET /decode 仍为 405。
	resp, err = http.Get(srv.URL + "/decode")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /decode = %d, want 405", resp.StatusCode)
	}
}

// ---- 一次性黑盒验收：真实 HTTP Server，混合批次端到端 ----

func TestBlackBoxMixedBatchOverHTTP(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	ok := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	checksumBad := withChecksum(ok, byte('0'+(int(ok[20]-'0')+1)%10))
	rangeBad := build(t, "TW", "KHI", "03", "15", "08", "30", "-", "5001", "UP")

	payload := fmt.Sprintf(
		`{"year":2026,"telegrams":[%q,%q,%q,"",%q]}`,
		ok, checksumBad, rangeBad, "TWKHI03150830+9999UPX")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decode-batch",
		bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q", resp.StatusCode, raw)
	}

	var got struct {
		Results []struct {
			Index      int    `json:"index"`
			Error      string `json:"error"`
			Station    string `json:"station"`
			ObservedAt string `json:"observed_at"`
			LevelMM    int    `json:"level_mm"`
			Trend      string `json:"trend"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	wantErrs := []string{"", statusChecksum, statusRange, statusFormat, statusFormat}
	if len(got.Results) != len(wantErrs) {
		t.Fatalf("got %d results, want %d (%s)", len(got.Results), len(wantErrs), raw)
	}
	for i, wantErr := range wantErrs {
		r := got.Results[i]
		if r.Index != i || r.Error != wantErr {
			t.Errorf("result %d = %+v, want error %q", i, r, wantErr)
		}
	}
	if got.Results[0].Station != "KHI" || got.Results[0].LevelMM != 1234 ||
		got.Results[0].ObservedAt != "2026-03-15T08:30:00Z" || got.Results[0].Trend != "UP" {
		t.Errorf("success result mismatch: %+v", got.Results[0])
	}

	// 非法批次经真实服务器整体拒绝。
	for _, bad := range []string{
		`{"year":1999,"telegrams":["x"]}`,
		`{"year":2026,"telegrams":[]}`,
		`{"year":2026,"telegrams":["x"],"telegrams":["y"]}`,
		`not json`,
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decode-batch",
			strings.NewReader(bad))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || string(b) != statusFormat {
			t.Errorf("batch %q: got %d %q, want 400 FORMAT_ERROR", bad, resp.StatusCode, b)
		}
	}
}
