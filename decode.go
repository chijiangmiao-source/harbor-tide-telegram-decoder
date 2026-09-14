package main

import "time"

// 错误码：判定顺序固定为 FORMAT_ERROR -> CHECKSUM_ERROR -> RANGE_ERROR。
const (
	statusFormat   = "FORMAT_ERROR"
	statusChecksum = "CHECKSUM_ERROR"
	statusRange    = "RANGE_ERROR"
)

// telegram 是潮位电报解译成功后的结构化结果。
type telegram struct {
	Station    string
	ObservedAt time.Time
	LevelMM    int
	Trend      string
}

// decodeTelegram 解译一行 21 字符的紧凑潮位电报。
//
// 布局：TTSSSMMDDhhmmSvvvvRRC（末位 C 为前 20 个字符 ASCII 码值之和模 10）。
// year 来自 X-Observation-Year 头（调用方保证是四位十进制数字）。
//
// 判定顺序：字符布局与日期时间错误 -> FORMAT_ERROR；校验位不符 ->
// CHECKSUM_ERROR；潮位越界 -> RANGE_ERROR。任何失败都只返回错误码字符串。
func decodeTelegram(line string, year int) (telegram, string) {
	t := telegram{}

	// 1. 请求内容的字符布局：恰好 21 个可打印 ASCII 字符、单行。
	if len(line) != 21 {
		return t, statusFormat
	}
	for i := 0; i < 21; i++ {
		b := line[i]
		if b < 0x20 || b > 0x7E {
			return t, statusFormat
		}
	}

	if line[0] != 'T' || line[1] != 'W' {
		return t, statusFormat
	}
	for i := 2; i < 5; i++ {
		if !isUpperLetter(line[i]) {
			return t, statusFormat
		}
	}
	for _, i := range []int{5, 6, 7, 8, 9, 10, 11, 12, 14, 15, 16, 17, 20} {
		if !isDigit(line[i]) {
			return t, statusFormat
		}
	}
	if line[13] != '+' && line[13] != '-' {
		return t, statusFormat
	}
	rr := line[18:20]
	if rr != "UP" && rr != "DN" && rr != "EQ" {
		return t, statusFormat
	}

	// 2. 年份范围与日期时间真实性（必须先于校验位判定）。
	if year < 2000 || year > 2099 {
		return t, statusFormat
	}
	month := twoDigits(line[5], line[6])
	day := twoDigits(line[7], line[8])
	hour := twoDigits(line[9], line[10])
	minute := twoDigits(line[11], line[12])

	obs := time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
	if obs.Year() != year || int(obs.Month()) != month || obs.Day() != day ||
		obs.Hour() != hour || obs.Minute() != minute {
		return t, statusFormat
	}

	// 3. 校验位：前 20 个字符 ASCII 码值之和模 10。
	var sum int
	for i := 0; i < 20; i++ {
		sum += int(line[i])
	}
	if sum%10 != int(line[20]-'0') {
		return t, statusChecksum
	}

	// 4. 潮位范围（校验通过后才判定）。
	level := fourDigits(line[14], line[15], line[16], line[17])
	if line[13] == '-' {
		level = -level // -0000 归一化为 0
	}
	if level < -5000 || level > 5000 {
		return t, statusRange
	}

	t.Station = line[2:5]
	t.ObservedAt = obs
	t.LevelMM = level
	t.Trend = rr
	return t, ""
}

func isDigit(b byte) bool       { return b >= '0' && b <= '9' }
func isUpperLetter(b byte) bool { return b >= 'A' && b <= 'Z' }

func twoDigits(a, b byte) int { return int(a-'0')*10 + int(b-'0') }

func fourDigits(a, b, c, d byte) int {
	return int(a-'0')*1000 + int(b-'0')*100 + int(c-'0')*10 + int(d-'0')
}
