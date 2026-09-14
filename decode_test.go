package main

import (
	"fmt"
	"testing"
)

// build 按字段拼装 20 位报文并补上校验位。
func build(t *testing.T, tt, st, mm, dd, hh, min, sign, vvvv, rr string) string {
	t.Helper()
	head := tt + st + mm + dd + hh + min + sign + vvvv + rr
	if len(head) != 20 {
		t.Fatalf("test helper: prefix length = %d, want 20", len(head))
	}
	sum := 0
	for i := 0; i < 20; i++ {
		sum += int(head[i])
	}
	return head + string(byte('0'+sum%10))
}

// withChecksum 返回按给定字符 C 替换末位的报文（用于构造错误校验位）。
func withChecksum(telegram string, c byte) string {
	return telegram[:20] + string(c)
}

func TestDecodeOK(t *testing.T) {
	cases := []struct {
		name                    string
		station, mm, dd, hh, mi string
		sign, vvvv, rr          string
		year                    int
		wantLevel               int
	}{
		{"positive", "KHI", "03", "15", "08", "30", "+", "1234", "UP", 2026, 1234},
		{"negative", "NAG", "12", "01", "23", "59", "-", "4321", "DN", 2030, -4321},
		{"equal trend", "XYZ", "07", "04", "00", "00", "+", "0050", "EQ", 2050, 50},
		{"minus zero normalizes", "ABC", "01", "01", "12", "00", "-", "0000", "EQ", 2024, 0},
		{"plus zero", "ABC", "01", "01", "12", "00", "+", "0000", "EQ", 2024, 0},
		{"lower range bound", "ABC", "06", "30", "06", "15", "-", "5000", "DN", 2025, -5000},
		{"upper range bound", "ABC", "06", "30", "06", "15", "+", "5000", "UP", 2025, 5000},
		{"leap day", "ABC", "02", "29", "09", "00", "+", "0001", "EQ", 2024, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := build(t, "TW", tc.station, tc.mm, tc.dd, tc.hh, tc.mi, tc.sign, tc.vvvv, tc.rr)
			if len(raw) != 21 {
				t.Fatalf("length = %d, want 21", len(raw))
			}
			got, code := decodeTelegram(raw, tc.year)
			if code != "" {
				t.Fatalf("unexpected error %s for %q", code, raw)
			}
			if got.Station != tc.station {
				t.Errorf("station = %q, want %q", got.Station, tc.station)
			}
			if got.LevelMM != tc.wantLevel {
				t.Errorf("level = %d, want %d", got.LevelMM, tc.wantLevel)
			}
			if got.Trend != tc.rr {
				t.Errorf("trend = %q, want %q", got.Trend, tc.rr)
			}
			wantStamp := fmt.Sprintf("%04d-%s-%sT%s:%s:00Z",
				tc.year, tc.mm, tc.dd, tc.hh, tc.mi)
			if s := got.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"); s != wantStamp {
				t.Errorf("observed at = %q, want %q", s, wantStamp)
			}
		})
	}
}

func TestFormatErrors(t *testing.T) {
	good := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")

	cases := []struct {
		name string
		raw  string
		year int
	}{
		{"too short", good[:20], 2026},
		{"too long", good + "9", 2026},
		{"empty", "", 2026},
		{"wrong tag", "TB" + good[2:], 2026},
		{"lowercase station", good[:2] + "khx" + good[5:], 2026},
		{"digit in station", good[:2] + "KH1" + good[5:], 2026},
		{"non ascii month", "TWKHI\xc3\xa9" + good[7:], 2026},
		{"control char in level", good[:14] + "\x01" + good[15:], 2026},
		{"newline at end", good + "\n", 2026},
		{"cr inside", good[:10] + "\r" + good[11:], 2026},
		{"non numeric month", good[:5] + "AB" + good[7:], 2026},
		{"month zero", "TWKHI" + "00" + good[7:], 2026},
		{"month thirteen", "TWKHI" + "13" + good[7:], 2026},
		{"day zero", good[:7] + "00" + good[9:], 2026},
		{"day beyond month", build(t, "TW", "KHI", "02", "30", "08", "30", "+", "1234", "UP"), 2026},
		{"not leap day", build(t, "TW", "KHI", "02", "29", "08", "30", "+", "1234", "UP"), 2023},
		{"hour 24", good[:9] + "24" + good[11:], 2026},
		{"minute 60", good[:11] + "60" + good[13:], 2026},
		{"bad sign", good[:13] + "x" + good[14:], 2026},
		{"bad trend lowercase", good[:18] + "up" + good[20:], 2026},
		{"bad trend code", good[:18] + "XX" + good[20:], 2026},
		{"non digit checksum", good[:20] + "A", 2026},
		{"year too low", good, 1999},
		{"year too high", good, 2100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, code := decodeTelegram(tc.raw, tc.year)
			if code != statusFormat {
				t.Errorf("got %q, want FORMAT_ERROR (raw=%q)", code, tc.raw)
			}
		})
	}
}

func TestChecksumError(t *testing.T) {
	good := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "1234", "UP")
	// 末位加 1（模 10），其余字段完全合法。
	wrong := withChecksum(good, byte('0'+(int(good[20]-'0')+1)%10))
	_, code := decodeTelegram(wrong, 2026)
	if code != statusChecksum {
		t.Fatalf("got %q, want CHECKSUM_ERROR", code)
	}
}

func TestRangeError(t *testing.T) {
	for _, sign := range []string{"+", "-"} {
		raw := build(t, "TW", "KHI", "03", "15", "08", "30", sign, "5001", "UP")
		_, code := decodeTelegram(raw, 2026)
		if code != statusRange {
			t.Errorf("sign %s: got %q, want RANGE_ERROR", sign, code)
		}
	}
	// 9999 且校验正确也必须是 RANGE_ERROR。
	raw := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "9999", "UP")
	if _, code := decodeTelegram(raw, 2026); code != statusRange {
		t.Errorf("9999: got %q, want RANGE_ERROR", code)
	}
}

func TestDecisionOrder(t *testing.T) {
	// 越界 + 校验位错误：先报 CHECKSUM_ERROR。
	bad := build(t, "TW", "KHI", "03", "15", "08", "30", "+", "9999", "UP")
	bad = withChecksum(bad, byte('0'+(int(bad[20]-'0')+1)%10))
	if _, code := decodeTelegram(bad, 2026); code != statusChecksum {
		t.Errorf("range+checksum: got %q, want CHECKSUM_ERROR", code)
	}

	// 日期不存在 + 校验位错误：先报 FORMAT_ERROR。
	bad = build(t, "TW", "KHI", "02", "30", "08", "30", "+", "9999", "UP")
	bad = withChecksum(bad, '0')
	if _, code := decodeTelegram(bad, 2026); code != statusFormat {
		t.Errorf("date+checksum+range: got %q, want FORMAT_ERROR", code)
	}
}
