package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func servePost(t *testing.T, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/decode", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	decodeHandler(rec, req)
	return rec
}

func validBody(t *testing.T) string {
	t.Helper()
	return build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
}

func TestHandlerSuccess(t *testing.T) {
	rec := servePost(t, validBody(t), map[string]string{
		"X-Observation-Year": "2026",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var got successResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	if got.Station != "KHI" || got.LevelMM != 1234 || got.Trend != "UP" {
		t.Errorf("unexpected payload: %+v", got)
	}
	if got.ObservedAt != "2026-03-15T08:30:00Z" {
		t.Errorf("observed_at = %q, want UTC RFC3339", got.ObservedAt)
	}
}

func TestHandlerMinusZero(t *testing.T) {
	body := build(t, "TW", "NAG", "12", "31", "23", "05", "-", "0000", "EQ")
	rec := servePost(t, body, map[string]string{"X-Observation-Year": "2099"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var got successResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.LevelMM != 0 {
		t.Errorf("level = %d, want 0", got.LevelMM)
	}
	if got.ObservedAt != "2099-12-31T23:05:00Z" {
		t.Errorf("observed_at = %q", got.ObservedAt)
	}
}

func TestHandlerErrors(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		headers    map[string]string
		contentTyp string
		wantCode   string
	}{
		{
			name:       "missing year header",
			body:       validBody(t),
			contentTyp: "text/plain",
			wantCode:   statusFormat,
		},
		{
			name:       "year out of range",
			body:       validBody(t),
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "1999"},
			wantCode:   statusFormat,
		},
		{
			name:       "year non numeric",
			body:       validBody(t),
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "20ab"},
			wantCode:   statusFormat,
		},
		{
			name:       "wrong content type",
			body:       validBody(t),
			contentTyp: "application/json",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name:       "non utf8 charset",
			body:       validBody(t),
			contentTyp: "text/plain; charset=gbk",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name:       "no content type",
			body:       validBody(t),
			contentTyp: "",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name:       "bad layout",
			body:       "TWKHI03150830+1234UP",
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name:       "trailing newline",
			body:       validBody(t) + "\n",
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name:       "bad date",
			body:       build(t, "TW", "KHI", "02", "30", "08", "30", "+", "1234", "UP"),
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusFormat,
		},
		{
			name: "bad checksum",
			body: withChecksum(validBody(t),
				byte('0'+(int(validBody(t)[20]-'0')+1)%10)),
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusChecksum,
		},
		{
			name:       "range error",
			body:       build(t, "TW", "KHI", "03", "15", "08", "30", "-", "5001", "UP"),
			contentTyp: "text/plain",
			headers:    map[string]string{"X-Observation-Year": "2026"},
			wantCode:   statusRange,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/decode", strings.NewReader(tc.body))
			if tc.contentTyp != "" {
				req.Header.Set("Content-Type", tc.contentTyp)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			decodeHandler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			// 失败响应只能包含错误码本身，不得泄露任何报文字段。
			if rec.Body.String() != tc.wantCode {
				t.Errorf("body = %q, want exactly %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/decode", nil)
	rec := httptest.NewRecorder()
	decodeHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandlerCharsetUTF8Accepted(t *testing.T) {
	body := validBody(t)
	req := httptest.NewRequest(http.MethodPost, "/decode", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	req.Header.Set("X-Observation-Year", "2026")
	rec := httptest.NewRecorder()
	decodeHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandlerOversizedBody(t *testing.T) {
	big := strings.Repeat("A", 5000)
	req := httptest.NewRequest(http.MethodPost, "/decode", strings.NewReader(big))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Observation-Year", "2026")
	rec := httptest.NewRecorder()
	decodeHandler(rec, req)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != statusFormat {
		t.Fatalf("got %d %q, want 400 FORMAT_ERROR", rec.Code, rec.Body.String())
	}
}

// 端到端：经真实 httptest.Server 发出字节级请求，确认非 ASCII 字节也会被拒。
func TestHandlerNonASCIIBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(decodeHandler))
	defer srv.Close()

	raw := []byte(build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP"))
	raw[5] = 0xC3 // 月份位放入非 ASCII 字节（仍是 21 字节）
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decode", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Observation-Year", "2026")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || string(b) != statusFormat {
		t.Fatalf("got %d %q, want 400 FORMAT_ERROR", resp.StatusCode, string(b))
	}
}
