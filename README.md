# Zen Gate Server

> **把 OpenCode Zen 免费模型装进你所有的 AI Agent —— Linux 服务端版，带 WebUI。**

在原 [zen-gate](https://github.com/LAGcomcom/zen-gate)（Windows 托盘程序）基础上重写的 **Linux 无头服务端**：
单二进制、systemd 友好、Docker 就绪，用浏览器管理，不做任何桌面依赖。

在服务器上把 OpenCode Zen 免费车道封装成**标准 OpenAI / Anthropic / Responses 兼容接口**，
任何支持自定义 base_url 的客户端都能直接接入。

---

## 与原版的区别

| | 原版（Windows 托盘） | 本版（Linux 服务端） |
|---|---|---|
| 运行形态 | 桌面托盘 + WebView2 窗口 | 无头服务 + 浏览器 WebUI |
| 监听地址 | 强制 127.0.0.1 | 可配（默认回环，可绑 0.0.0.0 供局域网/容器） |
| 配置注入 | 自动探测并改写本机 10+ Agent 配置 | 不做（服务端不碰客户端），提供各客户端接入示例 |
| 系统代理 | 读 Windows 注册表 WinINET | 读 gsettings / KDE，或环境变量 |
| 数据目录 | `%APPDATA%\zen-gate` | `$XDG_DATA_HOME/zen-gate` |
| 更新机制 | 自更新二进制 | 交给包管理 / systemd / Docker 镜像 |
| 认证 | 仅回环护栏 | API Key（`/v1`）+ WebUI 密码（管理页） |
| 协议核心 | — | **完全一致**（移植自同一 MIT 参考实现） |

---

## 功能

- 🔄 **限流自动切换**：模型被限流时自动换下一个可用模型接住请求，响应头 `x-zen-gate-served-by` 标注实际服务模型
- 🌐 **代理池自动轮换**：额度按 IP 计算，本机用完自动换免费代理接续——抓取 → 测速 → 按延迟排序 → 只挑连得通的
- 🧬 **协议完整移植**：会话铸造、指纹门（tools 四元组）、三种线协议（chat / responses / messages）、纯思考断流恢复、DSML 控制标记清洗
- 📊 **额度测算**：无官方余额 API 也能估——限额时段追踪 + 恢复时间预估 + 日额度进度条
- ⏱ **首字历史**：每次探测的首字延迟入样本环，重启不丢，模型页直接看平均首字
- 🌡 **GitHub 式热力图**：17 周用量热力图 + 30 天趋势柱状图 + 模型用量占比
- 🩺 **单模型体检**：每个模型可单独探测可用性与首字延迟
- 🖥 **完整 WebUI**：总览、模型、代理池、统计、共享、日志、设置、接入 八个页面，实时轮询 + 一键复制配置片段
- 🐳 **零依赖部署**：纯 Go 标准库，无 CGO，单文件二进制

---

## 快速开始

### 0. 获取代码

```bash
git clone https://github.com/sanhe458/zen-gate-server.git
cd zen-gate-server
```

或从 [Releases](https://github.com/sanhe458/zen-gate-server/releases) 直接下载预编译二进制：

```bash
# linux/amd64
curl -L -o zen-gate-server https://github.com/sanhe458/zen-gate-server/releases/latest/download/zen-gate-server-linux-amd64
chmod +x zen-gate-server && ./zen-gate-server
```

### 1. 构建

```bash
make build            # → dist/zen-gate-server
# 或交叉编译 linux/amd64 + linux/arm64：
make release
```

需要 Go 1.25+（纯标准库，无第三方依赖）。

### 2. 运行

```bash
# 前台调试（仅本机可访问）
./dist/zen-gate-server

# 指定端口与数据目录
./dist/zen-gate-server -port 8787 -data /var/lib/zen-gate
```

启动后：

- **对外展示页**：`http://127.0.0.1:8787/` —— 直接把这个地址发给别人即可，页面会按对方访问的域名显示对应接入地址
- **管理台**：`http://127.0.0.1:8787/admin`
- **API**：`http://127.0.0.1:8787/v1` —— 不带 Key 也能调用（限 60 次/分钟）
- **主 API Key**：首次启动自动生成 `ofm-…`，在管理台「设置 → API Key」查看或轮换

### 3. 命令行参数

| 参数 | 说明 |
|---|---|
| `-host` | 监听地址。默认 `127.0.0.1`；`0.0.0.0` 暴露局域网 |
| `-port` | 监听端口，默认 8787 |
| `-data` | 数据目录，默认 `$ZEN_GATE_HOME` 或 `~/.local/share/zen-gate` |
| `-password` | 设置 WebUI 密码（远程访问管理页必需） |
| `-version` | 打印版本退出 |

环境变量：`ZEN_GATE_HOME`（数据目录）、`http_proxy` / `https_proxy`（代理模式下生效）。

---

## 部署

### systemd

```bash
sudo useradd -r -s /usr/sbin/nologin zen-gate
sudo mkdir -p /var/lib/zen-gate && sudo chown zen-gate:zen-gate /var/lib/zen-gate
sudo install -m755 dist/zen-gate-server /usr/local/bin/zen-gate-server
sudo cp deploy/zen-gate-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now zen-gate-server
journalctl -u zen-gate-server -f
```

### Docker

```bash
docker build -t zen-gate-server -f deploy/Dockerfile .
docker run -d --name zen-gate -p 127.0.0.1:8787:8787 \
  -v zen-gate-data:/data zen-gate-server
```

### 开放局域网 / 远程管理

服务端默认只监听回环。要让同网段其他机器使用：

1. WebUI「设置 → 监听与安全」把监听地址改为 `0.0.0.0`，**设置 WebUI 密码**，保存后重启服务；
2. 或用命令行：`zen-gate-server -host 0.0.0.0 -password 'your-strong-password'`。

> ⚠️ 绑定 `0.0.0.0` 且未设密码时：`/v1` API 仍受 API Key 保护，但**管理页会拒绝非本机访问**（安全默认）。

---

## 对外展示页 与 管理台

![对外展示页](docs/screenshot-public.png)

| 路径 | 内容 | 访问控制 |
|---|---|---|
| `/` | 对外展示页：接入地址、调用示例、可用模型 | 公开 |
| `/admin` | 管理台（总览/模型/代理池/统计/共享/日志/设置/接入） | 回环免密，远程需密码 |
| `/public-info.json` | 展示页的数据源 | 公开（无任何密钥） |

**接入地址按你实际访问的地址实时生成**——用域名访问就显示域名，用 IP 就显示 IP，HTTPS 访问显示 `https://`。反向代理场景会自动识别（仅当直连来源是回环/内网时才采信 `X-Forwarded-Proto` / `X-Forwarded-Host` / `X-Forwarded-Prefix`，否则公网客户端可以伪造头）。

所以同一份部署，在 `https://api.example.com/` 上打开就显示 `https://api.example.com/v1`，在 `http://127.0.0.1:8787/` 上打开就显示 `http://127.0.0.1:8787/v1`，示例代码、curl、各客户端配置片段全部随之同步。

## 免 Key 调用与限速

`/v1` 允许**不携带 API Key** 直接调用，按来源 IP 限速：

| 调用方式 | 限制 |
|---|---|
| 不带 Key | 每 IP **60 次/分钟**（默认，可在管理台调整，填 0 则关闭免 Key 调用） |
| 带有效 Key | **无限制** |
| 带错误 Key | `401` —— 不会静默降级成匿名，避免打错字被误读成限速 |

响应头会主动告知预算：`X-RateLimit-Limit`、`X-RateLimit-Remaining`；超限返回 `429` 并附 `Retry-After`：

```json
{"error":{"code":"anonymous_rate_limited",
          "message":"匿名调用限速 60 次/分钟，请稍后重试，或使用 API Key 解除限制",
          "type":"rate_limit_error"}}
```

用**滑动窗口**而非固定分钟桶：固定桶会让调用方在某一分钟末尾和下一分钟开头各刷满一次，实际速率翻倍。

匿名调用在统计里单独归到 `anonymous`，与主 Key、共享 Key 的用量分开记账。

> **反代部署提醒**：限速依赖真实客户端 IP。本服务只在直连来源是回环或内网地址时才采信 `X-Forwarded-For` / `X-Real-IP`，所以请确保你的反向代理**覆盖**（而不是追加）这些头——例如 Nginx 用 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` 与 `proxy_set_header Host $host;`。

## WebUI

六个页面，全部走 `/admin/api/*`，回环调用免密、远程调用需会话 cookie。

![总览](docs/screenshot-overview.png)
![模型](docs/screenshot-models.png)
![代理池](docs/screenshot-proxypool-list.png)

| 页面 | 内容 |
|---|---|
| **总览** | 可用模型数、今日请求/输出、网关地址、出口 IP 与地区（含"经由代理"标识）、服务信息、最近 40 条调用记录 |
| **模型** | 每模型状态徽章（可用/已限额/地区受限）、首字与平均首字、上下文与输出上限、社区简介、额度进度条、单模型「测试」按钮 |
| **代理池** | 当前出口、可用代理数、设置（启用/自动切换/测试参数/源列表）、刷新·切换·切回直连、可用节点表格（延迟排序） |
| **统计** | 近 30 天请求量与输出 Tokens 柱状图、模型用量占比、17 周用量热力图、CSV 导出 |
| **共享** | 可发放的独立 Key（`ofs-` 前缀）与各自用量明细 |
| **日志** | 内存环形缓冲（最近 1000 条），按级别过滤，可自动刷新 |
| **设置** | 默认输出预算 / 思考力度 / 探测间隔 / 限流切换 / 地区信息；代理模式与连通性测试；监听端口地址与 WebUI 密码；API Key 轮换；配置导入导出 |
| **接入** | 连接信息 + 各客户端可直接复制的配置片段（curl / Python / Claude Code / Codex / OpenCode）与模型速查表 |

---

## 代理池（额度耗尽自动换 IP）

免费车道按 **IP** 计算每日额度。本机直连用完后，换一个出口 IP 就能继续用——这就是代理池要解决的问题。

### 工作方式

```
启动 ──▶ 本机直连（不使用任何代理）
           │
           ├── 额度不足信号 ────────────────────────┐
           │   ① 某次请求返回 429 / FreeUsageLimitError │
           │   ② 每 15 分钟探测中出现「整轮 429」        │
           │      （IP 级额度墙）                      │
           │                                          ▼
           │                            从池中挑「延迟最低 且
           │                            已验证可用」的代理接替
           │                                          │
           └── 代理失效信号 ──┐                        │
               请求根本没到上游 │                        │
               （连接被拒/重置/超时）│                     │
                        ▼                            │
             先验证当前代理是否还活着 ── 活着 ──▶ 判定上游抖动，不切换
                        │                            │
                       死                        ┌───┘
                        │                        │
                        └────────────────────────┤
                                                 ▼
                          重置该 IP 的全部限额状态，立即重新探测模型
                                                 │
                          该代理再次耗尽/失效 ──▶ 换下一个 ──▶ 池空则补充
```

两种信号的语义不同，处理方式也不同：

| 信号 | 含义 | 处理 |
|---|---|---|
| **额度耗尽**（429） | 代理是好的，只是这个 IP 今天用完了 | 标记「额度已耗尽」，换下一个 |
| **代理失效**（连不上） | 代理本身挂了，与额度无关 | **先验证再切换**：验证仍可达 → 判定上游抖动，不动；验证失败 → 标记失效并切换 |

- **始终先走本机直连**，只有出现额度不足才启用代理；
- 同一个 20 秒窗口内只切换一次（一次请求可能在 failover 中产生多次 429）；
- 「先验证再切换」很重要：上游偶尔抖动也会表现为请求失败，无脑切换只会白白丢掉一个好节点；
- 切换后自动 `ClearThrottle` 并重新探测——配额是按 IP 记的，旧 IP 的限额结论对新 IP 无效。

### 后台自动维护

| 机制 | 默认 | 说明 |
|---|---|---|
| **定时全量刷新** | **每 10 分钟** | 重新拉取全部源 → 完整实测 → 出口 IP 查重，全程约 1 分钟，后台异步 |
| 池空提前补充 | 可用数 < 3 | 两次定时刷新之间如果池子见底，立刻补一次（最快每 5 分钟一次） |
| 快照恢复 | — | 重启后直接沿用上次的可用节点，不必等一轮实测 |

定时刷新周期可在 WebUI 调整，**填 0 则关闭**（只手动刷新）。

### 代理是怎么筛出来的

1. **抓取**：并发拉取 14 个公开代理列表 / API（内置，可替换），去重后随机抽样；
2. **测速**：并发对每个候选发起真实请求，默认要求**能访问 zen 上游**才算可用（可在设置里关掉，改成只测通用连通性，更快但可能选到不能用的）；
3. **排序**：可用的按延迟升序排列，轮换永远优先选最快的；
4. **测出口**：对通过的节点再问一次「我的出口 IP 是多少」，记下它**真实**的出口地址（见下文"为什么关心出口 IP"）；
5. **剔除**：
   - 连不通的 → 直接丢弃；
   - **TLS 证书校验失败的 → 判定为中间人窃听，一律拒绝**（这类代理会试图解密你的 HTTPS 流量）；
   - 返回 `429` 的 → 标记"该 IP 额度已耗尽"，不再使用（代理本身是通的，但对我们没用）；
   - 返回 `403` 的 → 标记"被地区/风控拦截"。

内置源清单：

| 源 | 类型 | 说明 |
|---|---|---|
| HankNovic/ProxyClean | 纯文本 | 持续保活 + 国内连通性实测的清洗 SOCKS5 池 |
| proxy.scdn.io | **API** | CDN 服务商公益池，每轮随机返回已校验节点 |
| TheSpeedX / monosans / proxifly / ErcinDedeoglu / sunny9577 / mmpx12 / ShiftyTR / roosterkid / prxchk / zloi-user / hookzof | 纯文本 | 通用公开 HTTP/SOCKS5 列表，量大但死节点多 |

**关于 API 型源**：`proxy.scdn.io` 的接口每轮只随机返回 20 个节点，所以在源地址后加 `#repeat=N` 让它连续请求 N 轮并合并结果（片段不会发给服务器）：

```
https://proxy.scdn.io/api/get_proxy.php?protocol=http&count=20#repeat=5
https://proxy.scdn.io/api/get_proxy.php?protocol=socks5&count=20#repeat=3
```

注意**协议要分开请求**（`protocol=http` / `protocol=socks5` 各一条）——用 `protocol=all` 会混入 SOCKS 类型，而按 HTTP 去测 SOCKS 代理只会得到误判。

任何返回 `ip:port` 的接口都能这样接：纯文本逐行、JSON 数组、`{"data":{"proxies":[...]}}`、`[{"ip":..,"port":..}]` 都能解析。

**抓不到的源用本地文件接**：有些站（例如 `proxy5.net`）整站套了 Cloudflare JS 挑战，程序化抓取拿不到数据——连真实浏览器都可能卡在挑战页。这类列表可以手动导出成文件，然后用 `file://` 源接入：

```
file:///var/lib/zen-gate/mylist.txt
file:///var/lib/zen-gate/proxy5-export.json
```

支持绝对/相对路径，纯文本与 JSON 同样解析，行尾 `#cc` 国家标注也认。这样任何来源的列表（自建代理、付费服务商面板、浏览器导出的表格）都能进池参与实测。

> **实测可用率**（从中国大陆网络访问 zen 上游）：免费代理整体约 **1%～5%**，且波动很大。
>
> | 源 | 候选 | 可用 | 可用率 |
> |---|---|---|---|
> | ProxyClean（单独） | 103 | 3 | 2.9% |
> | scdn.io API（单独） | 171 | 3 | 1.8% |
> | 全部源混合 | 1388 | 48 | 3.5% |
>
> 单轮结果受时段与网络状况影响很大（同样的配置有时 11 个、有时 48 个）。**多刷几轮会累积**，可用数比单轮结果更重要。

### 为什么关心出口 IP

免费代理列表里经常出现**同一个出口 IP 对应多个端口**的情况（实测 48 个可用节点只出自 15 个 IP）。额度是按 IP 算的，所以换到一个"不同的代理"如果出口 IP 没变，等于白换。

因此每轮测速会对通过的节点实测真实出口 IP，并在轮换时**优先挑选出口 IP 确实不同的节点**——只有当池里没有别的选择时才退而求其次。

### 累积与持久化

- 每轮刷新会把**上一轮已确认可用的节点放进候选头部重新验证**，而不是丢掉重来——多刷几轮可用数会累积；
- 测得的可用节点写入 `<数据目录>/proxypool.json`，**重启后直接恢复**，不必重新等一分钟；
- 重启后若可用数低于阈值，20 秒后自动补一轮；否则等 5 分钟周期检查再决定。

### 执行一次要多久

刷新分两段：**抓取**（并发拉源，5～15 秒）和**实测**（主要耗时）。实测的耗时几乎完全由这个公式决定：

```
实测耗时 ≈ (候选数 ÷ 测试并发) × 单代理超时
```

原因是免费代理绝大多数是"黑洞"——连得上但永不响应，只能等超时。实测数据（同一批源，从中国大陆）：

| 每轮测试上限 | 并发 | 超时 | 抓取 | 实测 | **合计** |
|---|---|---|---|---|---|
| 300 | 150 | 8s | 4.7s | 20.4s | **25s** |
| **1500（默认）** | **120（默认）** | **6s（默认）** | **3.4s** | **54.6s** | **58s** |
| 1500 | 150 | 8s | 12.1s | 57.3s | **69s** |
| 1500 | 400 | 5s | 15.0s | 18.6s | **34s** |

> 默认配置（1500 / 120 / 6s）实测 **58 秒**——抓取只占 3.4 秒，其余全是实测。

第三组的理论值是 `1500÷400×5s = 18.8s`，实测 18.6s——几乎完全吻合，印证了"绝大多数候选都会耗满超时"。

**出口 IP 去重本身不耗时**（纯内存比较，微秒级）。为去重提供依据的**出口 IP 探测**才是额外开销：它只对**通过实测的节点**再发一次请求（约 6 秒超时），所以增量约为

```
可用节点数 ÷ 并发 × 6s
```

可用节点通常只占候选的 1%～5%，且分散在各并发批次里，所以**通常被实测阶段吸收，几乎看不到额外时间**（上面三组的实测值已包含它）。

调参建议：

- **想更快**：提高并发 + 降低超时。1500 个从 69s 压到 34s 就是这么来的；
- **想更全**：超时别低于 5 秒。免费代理里确实有"慢但可用"的节点，超时太短会把它们误判为死节点；
- **并发别盲目拉高**：400 并发意味着同时开 400 个出站连接，对本地网络和出口带宽都有压力，200～300 是比较稳的区间；
- **可用数比单轮耗时更重要**：多刷几轮靠累积，比一次测更多更划算。

### WebUI 配置

「代理池」页可配置：

| 配置项 | 默认 | 说明 |
|---|---|---|
| 启用代理池 | 关 | 打开后代理模式转为「代理池」，起始仍是本机直连 |
| 额度不足自动切换 | 开 | 429 或整轮限额时自动换出口（20 秒冷却） |
| 最少可用代理数 | 3 | 低于此数量时后台自动刷新补充 |
| 每轮测试上限 | 1500 | 每轮抽多少候选测速（越大越慢，越可能找到可用节点） |
| 测试并发数 | 120 | 并发探测宽度 |
| 单代理超时（秒） | 6 | 超过即判失败 |
| 验证上游 | 开 | 要求代理能访问 zen 上游才算可用 |
| 代理源 | 内置 11 个 | 每行一个纯文本列表地址 |

同一页面还提供 **刷新代理池 / 立即切换出口 / 切回本机直连**，以及可用节点表格（地址、延迟、状态、**真实出口 IP**、详情、最近测试时间、一键「使用」）。

### 安全说明（重要）

免费代理的持有者可以看到经过它的流量元数据，甚至尝试解密 HTTPS。本项目对此有两道防线：

1. **强制 TLS 校验**：代理若无法通过证书验证，直接判定为中间人并拒绝使用——实测确实抓到过这类代理（会在列表里标注"TLS 证书校验失败（疑似中间人窃听，已拒绝）"）；
2. **不落敏感数据**：代理池只被用来转发对 zen 上游的请求，你的 API Key 不经过代理池存储。

即便如此，免费代理**不稳定也不可信**，建议：

- 优先使用**自建代理**或付费代理（设置里填「自定义代理」即可，无需代理池）；
- 不要把代理池用于任何敏感流量的出口。

### 注意事项

- 池中节点随时可能失效，切换后首个请求失败属正常现象，下一次额度不足会再切；
- 同一出口 IP 下可能挂着多个端口，表格里的「出口 IP」列可以帮你识别（轮换时会自动优先选不同出口）；
- 想快速看效果：点几次「刷新代理池」让可用数累积起来，再去用。

---

## 接入示例

把 `<BASE>` 换成 `http://127.0.0.1:8787/v1`，`<KEY>` 换成你的主 Key。

### OpenAI Chat Completions

```bash
curl <BASE>/chat/completions \
  -H "Authorization: Bearer <KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="<BASE>", api_key="<KEY>")
r = client.chat.completions.create(model="mimo-v2.6-flash-free",
                                   messages=[{"role":"user","content":"hello"}])
print(r.choices[0].message.content)
```

### Anthropic Messages（Claude 协议）

```bash
curl <BASE>/messages \
  -H "x-api-key: <KEY>" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash-free","max_tokens":8192,
       "messages":[{"role":"user","content":"用一句话解释 TCP 拥塞控制"}]}'
```

### Claude Code

```bash
export ANTHROPIC_BASE_URL=<BASE>
export ANTHROPIC_AUTH_KEY=<KEY>
export ANTHROPIC_MODEL=mimo-v2.6-flash-free
export ANTHROPIC_SMALL_FAST_MODEL=deepseek-v4-flash-free
claude
```

### Codex CLI

```toml
# ~/.codex/config.toml
model_provider = "zen_gate"
model = "mimo-v2.6-flash-free"

[model_providers.zen_gate]
name = "Zen Gate"
base_url = "<BASE>"
wire_api = "chat"
env_key = "ZEN_GATE_API_KEY"
```

```bash
export ZEN_GATE_API_KEY=<KEY>
codex
```

### OpenCode

```json
{
  "provider": {
    "zen_gate": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "<BASE>", "apiKey": "<KEY>" },
      "models": { "mimo-v2.6-flash-free": {}, "deepseek-v4-flash-free": {} }
    }
  }
}
```

### 思考力度

推理模型支持在**模型名后加力度后缀**，或传 `reasoning_effort`：

- `mimo-v2.6-flash-free (light)` — 轻量，最省额度
- `mimo-v2.6-flash-free` — 均衡（默认）
- `mimo-v2.6-flash-free (deep)` — 深思

`reasoning_effort` 接受 `light|balanced|deep|medium|high|minimal|low|none|off|xhigh`。

---

## API 端点

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/` | 否 | **对外展示页**：接入地址、调用示例、可用模型 |
| GET | `/public-info.json` | 否 | 展示页数据（只含公开信息，不含任何 Key） |
| GET | `/health` | 否 | 健康检查（Docker HEALTHCHECK 用） |
| GET | `/v1/models` | Key 或匿名 | OpenAI 风格模型列表（含力度变体） |
| POST | `/v1/chat/completions` | Key 或匿名 | OpenAI Chat Completions（流式/非流式） |
| POST | `/v1/responses` | Key 或匿名 | OpenAI Responses API |
| POST | `/v1/messages` | Key 或匿名 | Anthropic Messages API |
| GET | `/v1/codex-catalog` | 否 | Codex 远程模型目录（非敏感，故意免认证） |
| — | `/admin` | 会话 | **管理台**（8 个页面） |
| — | `/admin/api/*` | 会话 | 管理接口。常用：`state`（总览数据）、`settings`、`reprobe`、`probe/:id`、`logs`、`usage.csv`、`proxy-test`、`key/rotate`、`export`、`import` |
| GET | `/admin/api/pool` | 会话 | 代理池状态（当前出口、可用节点、统计） |
| POST | `/admin/api/pool/refresh` | 会话 | 重新抓取并测速（异步，约 1 分钟） |
| POST | `/admin/api/pool/rotate` | 会话 | 切换到下一个最优代理 |
| POST | `/admin/api/pool/use` | 会话 | 指定出口，`{"url":""}` 切回本机直连 |

`/v1/*` 支持 CORS，浏览器端应用可跨域调用。

---

## 数据目录

```
$ZEN_GATE_HOME/            (默认 ~/.local/share/zen-gate)
├── config.json            配置（含 MainKey、WebUI 密码、代理、监听地址、代理池）
├── stats.json             按天聚合的用量统计 + 最近 40 条调用
├── quota.json             各模型限额时段历史（用于恢复时间预估）
├── perf.json              各模型首字延迟样本环（30 个）
├── proxypool.json         已验证可用的代理节点快照（重启后直接恢复）
├── backups/               配置备份
└── logs/                  按天滚动的日志文件，保留 7 天
```

配置全部为原子写入（先写 `.tmp` 再 rename），损坏文件会改名保留以便排查。

---

## 工作原理

上游 OpenCode Zen 免费车道没有标准 API Key 体系，一般客户端接不上。本服务在本机把这条车道封装成标准接口，关键点：

1. **会话铸造**：上游按 session 计额度，同一会话必须落在同一 session id 上（跨重启也稳定），否则会迅速触发 429；
2. **指纹门**：免费层要求 `tools` 中声明 `bash` / `glob` / `grep` / `read` 四个小写工具名。已有真实工具会被就地规范命名，没有的用 `pwsh→bash` 之类真实工具顶替，其余填自禁用占位；返回时再还原成调用方的原始名字；
3. **三种线协议**：`chat`（多数模型）、`responses`（muse-spark 系列）、`messages`（union-alpha）、`systemone`（jev 判定模型，非聊天），按模型自动选择上游端点，对下游统一暴露；
4. **断流恢复**：纯思考回合被上游切断时，用 checkpoint 续写并拿到最终答案；
5. **限流切换**：429 时按优先级换模型重试，并在响应头标注实际服务模型，错误体附带 `suggestions` 便于客户端立即改模型重试。

> 协议行为移植自 MIT 协议参考实现 [dsh-our-free-model](https://github.com/zouyuxuan122/dsh-our-free-model)；免费车道的使用仍受上游提供方条款约束。

---

## 开发

```bash
make vet       # go vet
make test      # go test ./...
make fmt       # go fmt
make build     # 构建 dist/zen-gate-server
make release   # 交叉编译 amd64 + arm64
make docker    # 构建镜像
make help      # 全部目标
```

项目结构：

```
cmd/zen-gate-server/     服务端入口（flags、生命周期、优雅退出、代理池接线）
internal/lane/           上游协议层：会话/指纹/流解码/探测/限流（移植，核心）
  ├── proxypool.go       代理池：抓取、并发测速、延迟排序、轮换、快照持久化
  ├── sysproxy_linux.go  Linux 系统代理探测（gsettings / KDE）
  └── client.go          Lane：Complete/failover/探测循环/额度耗尽钩子
internal/server/         HTTP 服务层：路由、认证、管理 API、WebUI 嵌入
  ├── server.go          路由 + API Key 认证 + 会话认证 + 工具函数
  ├── admin.go           管理 API（含代理池）+ WebUI 嵌入
  ├── openai.go          OpenAI chat / Responses 协议转换
  ├── anthropic.go       Anthropic Messages 协议转换
  ├── sse.go             SSE 输出辅助
  └── web/index.html     单文件 WebUI 控制台
internal/store/          配置与统计持久化
internal/logx/           日志（按天文件 + 内存环形缓冲）
deploy/                  systemd unit + Dockerfile
```

测试：

```bash
go test ./...                                    # 单元测试（离线，秒级）
ZG_LIVE_TEST=1 go test ./internal/lane/ -run TestLiveProxyPoolFetch -v   # 真实抓取+测速
```

---

## 许可

MIT。上游免费车道的使用条款由提供方决定，请自行确认合规使用。
