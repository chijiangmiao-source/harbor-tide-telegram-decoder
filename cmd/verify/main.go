// Command verify 是针对已运行的 tidegram API 的一次性黑盒验收程序。
// 它只使用标准库，对 POST /decode 做一组真实 HTTP 请求，全部通过时以 0 退出，
// 任一不符即以非 0 退出并打印失败明细。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type check struct {
	name string
	fn   func(base string) error
}

var failures int

func main() {
	base := envOr("BASE_URL", "http://api:8080")
	flag.StringVar(&base, "base-url", base, "API base URL")
	flag.Parse()
	base = strings.TrimRight(base, "/")

	// 全部验收请求共用带超时的 client。
	http.DefaultClient.Timeout = 5 * time.Second
	client := &http.Client{Timeout: 5 * time.Second}
	if err := waitForAPI(client, base+"/healthz", 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "API 未就绪: %v\n", err)
		os.Exit(2)
	}

	checks := []check{
		{"正潮位成功响应字段精确", checkPositive},
		{"负潮位与 UTC 时刻", checkNegative},
		{"-0000 归一化为 0", checkMinusZero},
		{"范围边界 ±5000", checkBoundaries},
		{"闰年 2024-02-29 合法", checkLeapDay},
		{"非闰年 2026-02-29 拒绝", checkNonLeapDay},
		{"缺年份头 FORMAT_ERROR", checkMissingYear},
		{"年份 2100 FORMAT_ERROR", checkYearOutOfRange},
		{"非 text/plain FORMAT_ERROR", checkWrongContentType},
		{"含非 ASCII 字节 FORMAT_ERROR", checkNonASCII},
		{"带换行的 22 字节 FORMAT_ERROR", checkTrailingNewline},
		{"校验位错误 CHECKSUM_ERROR", checkBadChecksum},
		{"潮位 5001 RANGE_ERROR", checkRange},
		{"越界且校验错时先报 CHECKSUM", checkOrderChecksumBeforeRange},
		{"批量混合结果（成功/校验错/越界/格式错）", checkBatchMixed},
		{"批量空数组整批 400 FORMAT_ERROR", checkBatchEmpty},
		{"批量超过 100 条整批 400 FORMAT_ERROR", checkBatchTooMany},
		{"批量非法年份整批 400 FORMAT_ERROR", checkBatchBatchBadYear},
		{"批量非法 JSON 整批 400 FORMAT_ERROR", checkBatchMalformed},
		{"GET /decode-batch 返回 405", checkBatchMethodNotAllowed},
		{"GET 返回 405", checkMethodNotAllowed},
	}

	for _, c := range checks {
		if err := c.fn(base); err != nil {
			failures++
			fmt.Printf("FAIL  %s: %v\n", c.name, err)
			continue
		}
		fmt.Printf("PASS  %s\n", c.name)
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\n%d/%d 项验收失败\n", failures, len(checks))
		os.Exit(1)
	}
	fmt.Printf("\n全部 %d 项验收通过\n", len(checks))
}

func checkPositive(base string) error {
	body := telegram("TWKHI03150830+1234UP")
	resp, err := post(base+"/decode", "text/plain", "2026", []byte(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d, 响应 %q", resp.StatusCode, raw)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("响应不是 JSON: %v (%q)", err, raw)
	}
	if len(fields) != 4 {
		return fmt.Errorf("成功响应字段数 %d，必须恰好为 4 (%q)", len(fields), raw)
	}
	want := map[string]any{
		"station":     "KHI",
		"observed_at": "2026-03-15T08:30:00Z",
		"level_mm":    float64(1234),
		"trend":       "UP",
	}
	for k, v := range want {
		if fields[k] != v {
			return fmt.Errorf("字段 %s = %v, 期望 %v (%q)", k, fields[k], v, raw)
		}
	}
	return nil
}

func checkNegative(base string) error {
	body := telegram("TWNAG12312359-4321DN")
	resp, err := post(base+"/decode", "text/plain", "2099", []byte(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d, 响应 %q", resp.StatusCode, raw)
	}
	var got struct {
		ObservedAt string `json:"observed_at"`
		LevelMM    int    `json:"level_mm"`
		Trend      string `json:"trend"`
		Station    string `json:"station"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return err
	}
	if got.Station != "NAG" || got.LevelMM != -4321 || got.Trend != "DN" ||
		got.ObservedAt != "2099-12-31T23:59:00Z" {
		return fmt.Errorf("响应与期望不符: %+v", got)
	}
	return nil
}

func checkMinusZero(base string) error {
	body := telegram("TWABC06011200-0000EQ")
	resp, err := post(base+"/decode", "text/plain", "2024", []byte(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d, 响应 %q", resp.StatusCode, raw)
	}
	var got struct {
		LevelMM int `json:"level_mm"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return err
	}
	if got.LevelMM != 0 {
		return fmt.Errorf("level_mm = %d, -0000 必须归一化为 0", got.LevelMM)
	}
	return nil
}

func checkBoundaries(base string) error {
	for _, tc := range []struct {
		prefix string
		year   string
		want   int
	}{
		{"TWABC06300615-5000DN", "2025", -5000},
		{"TWABC06300615+5000UP", "2025", 5000},
	} {
		resp, err := post(base+"/decode", "text/plain", tc.year, []byte(telegram(tc.prefix)))
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: 状态码 %d, 响应 %q", tc.prefix, resp.StatusCode, raw)
		}
		var got struct {
			LevelMM int `json:"level_mm"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			return err
		}
		if got.LevelMM != tc.want {
			return fmt.Errorf("%s: level_mm = %d, 期望 %d", tc.prefix, got.LevelMM, tc.want)
		}
	}
	return nil
}

func checkLeapDay(base string) error {
	body := telegram("TWABC02290900+0001EQ")
	resp, err := post(base+"/decode", "text/plain", "2024", []byte(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("状态码 %d, 响应 %q", resp.StatusCode, raw)
	}
	return nil
}

func checkNonLeapDay(base string) error {
	return expectErrorCode(base, "2026",
		[]byte(telegram("TWABC02290900+0001EQ")), "text/plain", "FORMAT_ERROR")
}

func checkMissingYear(base string) error {
	body := []byte(telegram("TWKHI03150830+1234UP"))
	resp, err := post(base+"/decode", "text/plain", "", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return assertCode(resp, "FORMAT_ERROR")
}

func checkYearOutOfRange(base string) error {
	return expectErrorCode(base, "2100",
		[]byte(telegram("TWKHI03150830+1234UP")), "text/plain", "FORMAT_ERROR")
}

func checkWrongContentType(base string) error {
	return expectErrorCode(base, "2026",
		[]byte(telegram("TWKHI03150830+1234UP")), "application/json", "FORMAT_ERROR")
}

func checkNonASCII(base string) error {
	raw := []byte(telegram("TWKHI03150830+1234UP"))
	raw[5] = 0xC3 // 21 字节保持不变，但月份位是非 ASCII 字节
	return expectErrorCode(base, "2026", raw, "text/plain", "FORMAT_ERROR")
}

func checkTrailingNewline(base string) error {
	raw := append([]byte(telegram("TWKHI03150830+1234UP")), '\n')
	return expectErrorCode(base, "2026", raw, "text/plain", "FORMAT_ERROR")
}

func checkBadChecksum(base string) error {
	good := telegram("TWKHI03150830+1234UP")
	wrong := good[:20] + string(byte('0'+(good[20]-'0'+1)%10))
	return expectErrorCode(base, "2026", []byte(wrong), "text/plain", "CHECKSUM_ERROR")
}

func checkRange(base string) error {
	return expectErrorCode(base, "2026",
		[]byte(telegram("TWKHI03150830-5001UP")), "text/plain", "RANGE_ERROR")
}

func checkOrderChecksumBeforeRange(base string) error {
	// 潮位 9999（越界）但校验位也错：固定顺序要求先报 CHECKSUM_ERROR。
	bad := telegram("TWKHI03150830+9999UP")
	bad = bad[:20] + string(byte('0'+(bad[20]-'0'+1)%10))
	return expectErrorCode(base, "2026", []byte(bad), "text/plain", "CHECKSUM_ERROR")
}

func checkMethodNotAllowed(base string) error {
	resp, err := http.Get(base + "/decode")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		return fmt.Errorf("GET 状态码 %d, 期望 405", resp.StatusCode)
	}
	return nil
}

// ---- /decode-batch 批量接口验收 ----

type batchRequestBody struct {
	Year      int      `json:"year"`
	Telegrams []string `json:"telegrams"`
}

func checkBatchMixed(base string) error {
	ok := telegram("TWKHI03150830+1234UP")
	// 校验位错（其余字段合法）。
	badChecksum := ok[:20] + string(byte('0'+(ok[20]-'0'+1)%10))
	// 潮位越界（校验位正确）。
	badRange := telegram("TWKHI03150830-5001UP")
	// 格式错：空串。
	badFormat := ""

	payload, _ := json.Marshal(batchRequestBody{
		Year:      2026,
		Telegrams: []string{ok, badChecksum, badRange, badFormat},
	})
	resp, err := postJSON(base+"/decode-batch", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("状态码 %d, 单条失败不应整批失败; 响应 %q", resp.StatusCode, raw)
	}

	var got struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("响应不是 JSON: %v (%q)", err, raw)
	}
	if len(got.Results) != 4 {
		return fmt.Errorf("results 长度 %d, 必须与输入等长为 4", len(got.Results))
	}

	wantSuccess := map[string]any{
		"index":       float64(0),
		"station":     "KHI",
		"observed_at": "2026-03-15T08:30:00Z",
		"level_mm":    float64(1234),
		"trend":       "UP",
	}
	for k, v := range wantSuccess {
		if got.Results[0][k] != v {
			return fmt.Errorf("成功项字段 %s = %v, 期望 %v (%q)", k, got.Results[0][k], v, raw)
		}
	}
	if len(got.Results[0]) != 5 {
		return fmt.Errorf("成功项必须恰好 5 个字段: %v", got.Results[0])
	}

	wantErrs := []string{"", "CHECKSUM_ERROR", "RANGE_ERROR", "FORMAT_ERROR"}
	for i, wantErr := range wantErrs {
		item := got.Results[i]
		if item["index"] != float64(i) {
			return fmt.Errorf("第 %d 项 index = %v, 顺序必须与输入一致", i, item["index"])
		}
		if wantErr == "" {
			continue
		}
		// 失败项只含 index 和错误码，且不回显原报文。
		if len(item) != 2 || item["error"] != wantErr {
			return fmt.Errorf("第 %d 项 = %v, 期望仅 index+error=%s", i, item, wantErr)
		}
	}
	for _, tg := range []string{badChecksum, badRange} {
		if bytes.Contains(raw, []byte(tg)) {
			return fmt.Errorf("失败项不得回显原报文 %q", tg)
		}
	}
	return nil
}

func checkBatchEmpty(base string) error {
	return expectBatchRejected(base, 2026, []string{})
}

func checkBatchBatchBadYear(base string) error {
	return expectBatchRejected(base, 2100, []string{"x"})
}

func checkBatchTooMany(base string) error {
	items := make([]string, 101)
	ok := telegram("TWKHI03150830+1234UP")
	for i := range items {
		items[i] = ok
	}
	return expectBatchRejected(base, 2026, items)
}

func checkBatchMalformed(base string) error {
	resp, err := postJSON(base+"/decode-batch", []byte(`{"year":2026,"telegrams":[`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return assertCode(resp, "FORMAT_ERROR")
}

func checkBatchMethodNotAllowed(base string) error {
	resp, err := http.Get(base + "/decode-batch")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		return fmt.Errorf("GET /decode-batch 状态码 %d, 期望 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		return fmt.Errorf("Allow = %q, 期望 POST", allow)
	}
	return nil
}

func expectBatchRejected(base string, year int, items []string) error {
	payload, _ := json.Marshal(batchRequestBody{Year: year, Telegrams: items})
	resp, err := postJSON(base+"/decode-batch", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return assertCode(resp, "FORMAT_ERROR")
}

func postJSON(url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// ---- 辅助 ----

func expectErrorCode(base, year string, body []byte, contentType, want string) error {
	resp, err := post(base+"/decode", contentType, year, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return assertCode(resp, want)
}

func assertCode(resp *http.Response, want string) error {
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		return fmt.Errorf("状态码 %d, 期望 400; 响应 %q", resp.StatusCode, raw)
	}
	// 失败响应体必须且只能包含错误码本身。
	if string(raw) != want {
		return fmt.Errorf("响应体 %q, 期望恰好 %q", raw, want)
	}
	return nil
}

func post(url, contentType, year string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if year != "" {
		req.Header.Set("X-Observation-Year", year)
	}
	return http.DefaultClient.Do(req)
}

func waitForAPI(client *http.Client, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("健康检查状态码 %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

// telegram 为 20 字符前缀补上末位校验数字（ASCII 和模 10）。
func telegram(prefix string) string {
	if len(prefix) != 20 {
		panic(fmt.Sprintf("telegram 前缀长度 %d, 应为 20", len(prefix)))
	}
	sum := 0
	for i := 0; i < 20; i++ {
		sum += int(prefix[i])
	}
	return prefix + string(byte('0'+sum%10))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
