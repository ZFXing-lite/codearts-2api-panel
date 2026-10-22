# CodeArts 2API Panel

华为 CodeArts 大模型双渠道网关 + 账号池管理面板。聚合「国际版 CodeArts」与「国内码道 / 盘古助手」两个渠道的账号免费额度，统一对外提供 **OpenAI / Claude 双协议兼容 API**。

融合自 [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api)（国内码道渠道，感谢原作者开源）。

## ✨ 特性

- 🌏 **双渠道账号池**：国际版（snap-access 国际节点，GeeTest 自动注册）+ 国内码道（OAuth 登录、签到福利），统一调度、熔断、轮换
- 🔁 **智能调度**：配额感知轮换、per-account 并发上限、全局 RPS、健康号 sticky
- 🌐 **协议兼容**：OpenAI `/v1/chat/completions`、`/v1/responses`、Claude Messages 协议
- 🖥️ **Web 管理面板**：账号、代理池、API Key、配额仪表盘，另带独立的「国内码道」页（渠道状态、CN 账号表、签到记录、模型目录、运行参数快照、一键导入 auths）
- 🔐 **CN 登录工具链**：`cnlogin` 支持服务器无浏览器扫码登录（ticket 轮询）
- 🎁 **CN 自动化**：每日签到 / 限时福利自动领取、token 自动续期、保活心跳 watchdog
- 🗨️ **会话续接**：CN 渠道按账号维持 `chat_id` 上下文，多轮对话自动续接
- 🌍 **代理池**：账号级 HTTP / SOCKS5 代理绑定与连通性探测

## 🧭 渠道路由（realm 前缀）

模型名支持渠道前缀显式选道（借鉴 [workbuddy2api](https://github.com/ZFXing-lite/workbuddy2api) 的 realm 协议）：

| 模型名写法 | 路由 |
|---|---|
| `cn:snap-chat` / `cn:qwen3-vl-235b` | 只从国内码道账号选号，模型走 CN canonical 管线（不被国际别名改写） |
| `global:GLM-5.2`（旧写法 `intl:` 等价） | 只从国际账号选号 |
| `GLM-5.2`（无前缀） | 双亲调度：全池统一熔断/轮换/粘性，选中哪边就走哪边协议 |

`/v1/models` 返回国际目录 + `cn:` 前缀的 CN 账号已检出模型目录。CN 渠道总闸 `CN_ENABLED=false` 时 CN 账号整批不进可调集合。

## 🖥️ 面板：双渠道融合结构

侧栏按「通用 / 国际版 CodeArts / 国内码道 · 盘古助手」三组切分——**共用的融合，专属的分清**：

| 分组 | 页面 | 渠道归属 |
|---|---|---|
| 通用 | 总览 | 融合：双渠道账号数、Ready/透支分解、请求量、渠道健康卡 |
| 通用 | 模型目录 | 融合视图，内部按渠道分区（见下） |
| 通用 | API Key | 融合：可绑定全池账号，选号列表带 `CN` 徽标 |
| 国际版 | 国际版账户池 | 仅国际：txt 导入、注册、AK/SK、额度、代理绑定 |
| 国际版 | CLI 授权 | 仅国际：IAM 永久 AK/SK / OAuth 回调 |
| 国际版 | 代理池 | 仅国际：账号级 HTTP/SOCKS5 出站（国内码道走环境变量代理） |
| 国内码道 | 国内码道 | 仅国内：OAuth 账号、签到积分、续期保活、auths 导入 |

### 模型目录页

三个分区回答三个问题：谁能用、怎么调、来自哪。

| 分区 | 内容 |
|---|---|
| 路由规则 | 无前缀=双渠道调度、`cn:xxx`=仅国内、`global:xxx`=仅国际（`intl:` 同义） |
| 国际版模型 | 上游 snap-access 目录 + 各账号检出 + 内置兜底，标「上游目录 / 内置兜底」与可用账号数 |
| 国内码道模型 | CN 账号已检出目录，列 `cn:模型ID` 调用写法、可用账号数、福利标记 |
| 原始 /v1/models | 客户端实际拿到的合流结果（含 `cn:` 前缀条目） |

配套 API：`GET /api/models/catalog`（双渠道分区 + 账号计数）、`GET /api/cn/models`。

### 国内码道登录（三种部署各有路径）

华为云 portal 的回调目标是 `http://127.0.0.1:{port}/oauth/callback`，port 由授权链接的
`port` 参数决定，无法改成域名。所以：

| 场景 | 做法 |
|---|---|
| 浏览器与服务同机（本机跑 / docker 端口映射） | 面板「生成登录链接」→ 打开 → 登录，portal 直接回调本服务，自动完成 |
| 浏览器在另一台机器 | 登录后浏览器跳到 `127.0.0.1` 打不开是正常的，把地址栏那条 URL 整段粘到「粘贴回调地址」 |
| 反向代理暴露公网 | 配 `CN_OAUTH_CALLBACK_HOST=https://域名:端口`，授权链接的 port 会被改写，回调经反代进入 |

回调有两种形态，服务端都处理：只带 `secret` 时 307 送回 portal 继续走（否则第二次带
`code` 的回调不会发生）；带 `code` 时直接换 STS 凭证并入库。

`cnlogin -print-only` 走同一条粘贴通道——早期版本它只是空转轮询本地 secret，而上游
只认 portal 下发的 secret，所以必然 5 分钟超时。

### 国内码道页

| 区块 | 内容 |
|---|---|
| 面板登录 | 点「生成登录链接」→ 浏览器登录华为云账号，同机自动完成；跨机器把回调 URL 粘回来即可（见下） |
| CLI 登录 | `cnlogin`（本机）/ `cnlogin -print-only`（服务器，粘贴回调）→ `auths/codearts-*.json` → 一键导入 |
| 状态卡 | 渠道开关、账号数/可调用、待重登、今日签到进度、auths 凭证数 |
| CN 账号表 | 账号、UserID、调用态、Token 状态（含到期时间）、今日签到、最近活动、成功/失败 |
| 行操作 | `续期`、`签到`、`清重登标记` |
| 运行参数 | `CN_*` 全量只读快照（开关、目录、轮询、skew、保活、并发、超时、熔断、签到） |

配套 API：`GET /api/cn/dashboard`、`POST /api/cn/import`、`GET /api/cn/models`、
`POST /api/cn/accounts/{id}/refresh|checkin|relogin-clear`。

国际额度接口对 CN 账号自动降级（额度语义是积分/签到），避免误报。

## 🚀 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/ZFXing-lite/codearts-2api-panel.git
cd codearts-2api-panel
cp env.example .env      # 按需修改
docker compose up -d --build
```

打开 `http://127.0.0.1:29137` 登录面板。

### 源码运行

```bash
go build -o codearts ./cmd/server
./codearts
```

要求 Go 1.23+。

### 添加国内码道账号

```bash
# 服务器无浏览器：打印授权链接，任意机器浏览器打开授权
go run ./cmd/cnlogin -print-only

# 本机有浏览器
go run ./cmd/cnlogin
```

凭证落盘 `./auths/codearts-<user_id>.json`，服务启动时自动导入账号池。

## ⚙️ 配置

主要环境变量（详见 `env.example`）：

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `PORT` | 监听端口 | `29137` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | 面板管理员账号 | `admin` / `admin123` |
| `API_KEY` | 对外 API 鉴权 Key（留空则开放） | 空 |
| `DATA_DIR` | 数据目录 | Docker=`/data`（compose 挂载卷）/ 裸机=`./data` |
| `REG_AREA_CODE` | 国际渠道注册区域代码 | `OM` |
| `CN_ENABLED` | 启用国内码道渠道 | `true` |
| `CN_AUTH_DIR` | CN 凭证目录 | `./auths` |
| `CN_CREDITS_AUTO_CLAIM` | 自动签到/领取积分 | `true` |
| `CN_WATCH_POLL` | CN watchdog 轮询周期 | `30m` |

> ⚠️ 生产环境务必修改默认管理员密码。

## 🔌 API 使用

在面板创建 API Key 后，按 OpenAI 方式调用：

```bash
curl http://127.0.0.1:29137/v1/chat/completions \
  -H "Authorization: Bearer <你的Key>" \
  -H "Content-Type: application/json" \
  -d '{"model": "<模型名>", "messages": [{"role": "user", "content": "hi"}]}'
```

Claude Code 等 Anthropic 客户端：将 `ANTHROPIC_BASE_URL` 指向面板地址即可走 Messages 协议。CN 渠道多轮上下文可用请求头 `X-Codearts-Chat-Id: <chatId>` 显式指定会话。

## 📁 目录结构

```
cmd/server        网关入口
cmd/cnlogin       国内码道 OAuth 登录工具
internal/api      API 网关 / 调度 / 限流 / 熔断 / 双渠道分流
internal/cnup     国内码道上游客户端（DPoP / 签到 / 福利）
internal/cnwatch  CN watchdog（续期 / 签到 / 保活）
internal/huawei   国际渠道接入（OAuth、浏览器自动化、配额）
internal/webui    管理面板前端
internal/store    持久化存储（敏感字段加密）
internal/proxypool 代理池
```

## 📄 License

MIT

## ⚠️ 免责声明

本项目仅供技术研究与学习交流，请遵守相关平台的服务条款，使用者自负一切责任。
