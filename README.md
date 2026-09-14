# tidegram — 潮位电报解译 API

纯后端 HTTP 服务，将一行 21 字符的紧凑潮位电报解译为结构化潮位记录。
Go 1.25 + 标准库 `net/http`，无第三方依赖。

## 报文布局

```
TTSSSMMDDhhmmSvvvvRRC
```

| 片段  | 长度 | 含义                                                |
| ----- | ---- | --------------------------------------------------- |
| TT    | 2    | 固定标识 `TW`                                       |
| SSS   | 3    | 三位大写字母站码（A–Z）                             |
| MM    | 2    | 月（01–12）                                         |
| DD    | 2    | 日（必须是该年月真实存在的日期）                    |
| hh    | 2    | 时（00–23）                                         |
| mm    | 2    | 分（00–59）                                         |
| S     | 1    | 正负号 `+` / `-`                                    |
| vvvv  | 4    | 绝对潮位毫米（0000–5000；`-0000` 归一化为 0）       |
| RR    | 2    | 趋势：`UP` / `DN` / `EQ`                            |
| C     | 1    | 校验位：前 20 个字符 ASCII 码值之和模 10 的十进制数 |

- 请求必须是 UTF-8、单行、恰好 21 个 ASCII 字符（不允许 CR/LF/Tab 等控制字符）。
- 年份由请求头 `X-Observation-Year` 提供，须为 2000–2099。
- 输出的观测时刻一律为 UTC：`YYYY-MM-DDThh:mm:ssZ`。

## 接口

`POST /decode`

- 请求头：`Content-Type: text/plain`（`charset` 缺省或为 `UTF-8`）、`X-Observation-Year: 2026`
- 请求体：21 字符报文
- 成功 `200`：

```json
{
  "station": "KHI",
  "observed_at": "2026-03-15T08:30:00Z",
  "level_mm": 1234,
  "trend": "UP"
}
```

- 失败 `400`，响应体**仅**包含错误码本身（不泄露任何字段）：

| 错误码          | 触发条件                                   |
| --------------- | ------------------------------------------ |
| `FORMAT_ERROR`  | 请求形态、字符布局、年份或日期时间不合法   |
| `CHECKSUM_ERROR`| 格式合法但校验位不符                       |
| `RANGE_ERROR`   | 校验通过但潮位超出 -5000～5000 毫米        |

判定顺序固定：先 FORMAT，再 CHECKSUM，最后 RANGE（三者同时存在时只返回最先命中的错误码）。
其他方法请求 `/decode` 返回 `405`。

### `POST /decode-batch`

交接时一次核对同一年份的多份报文。

- 请求头：`Content-Type: application/json`（`charset` 缺省或为 `UTF-8`）
- 请求体：

```json
{
  "year": 2026,
  "telegrams": ["TWKHI03150830+1234UP5", "TWKHI03150830-5001UP3"]
}
```

- `year`：2000–2099 的整数；`telegrams`：1–100 条字符串，逐条直接复用同一解码函数。
- 成功 `200`，返回 `results` 数组，与输入**等长且顺序一致**，每项以 `index`（从 0 起）标识位置：

```json
{
  "results": [
    {
      "index": 0,
      "station": "KHI",
      "observed_at": "2026-03-15T08:30:00Z",
      "level_mm": 1234,
      "trend": "UP"
    },
    { "index": 1, "error": "RANGE_ERROR" }
  ]
}
```

- 成功项复用上面的四个业务字段；失败项**只**含 `index` 与 `error`
  （取值为既有错误码 `FORMAT_ERROR` / `CHECKSUM_ERROR` / `RANGE_ERROR`，判定顺序与单条接口一致）。
- 单条失败不阻断其余结果，失败项不回显原报文。
- 整批请求形态非法时直接 `400` 且响应体**仅**为 `FORMAT_ERROR`：
  JSON 结构错误、缺字段、未知/重复字段、顶层多余 JSON 值、空数组、超过 100 条、`year` 非整数或越界等。
- 其他方法请求 `/decode-batch` 返回 `405`（`Allow: POST`）。

## 示例

```sh
# 生成合法报文（末位校验位 = 前 20 字符 ASCII 和 mod 10）
prefix='TWKHI03150830+1234UP'
sum=0; for ((i=0;i<20;i++)); do printf -v c %d "'${prefix:$i:1}"; sum=$((sum+c)); done
msg="$prefix$((sum%10))"

curl -s http://localhost:8080/decode \
  -H 'Content-Type: text/plain' \
  -H 'X-Observation-Year: 2026' \
  --data-binary "$msg"

# 批量核对同一年份的多份电报（结果与输入等长、同序）
curl -s http://localhost:8080/decode-batch \
  -H 'Content-Type: application/json' \
  -d '{"year":2026,"telegrams":["TWKHI03150830+1234UP5","TWKHI03150830-5001UP3"]}'
```

## 运行（Docker Compose）

```sh
docker compose up --build -d api         # 启动 API（宿主默认端口 8080）
API_PORT=9090 docker compose up -d api   # 用 API_PORT 覆盖宿主端口
docker compose run --build --rm verify   # 运行一次性验收服务（21 项黑盒用例）
```

`verify` 是一次性服务：等待 `api` 健康检查通过后，对其发起真实 HTTP 验收请求，
全部通过退出码为 0，任一失败非 0。

## 本地开发与测试

```sh
go test ./... -v        # 单元测试 + httptest HTTP 测试（标准 testing）
go run .                # 默认监听 :8080，可用环境变量 PORT 覆盖
go run ./cmd/verify --base-url http://127.0.0.1:8080
```
