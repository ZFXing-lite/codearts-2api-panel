# CodeArts 2API Panel

华为 CodeArts 大模型双渠道网关 + 账号池管理面板。聚合「国际版 CodeArts」与「国内码道 / 盘古助手」两个渠道的账号免费额度，统一对外提供 **OpenAI / Claude 双协议兼容 API**。

融合自 [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api)（国内码道渠道，感谢原作者开源）。

## ✨ 特性

- 🌏 **双渠道账号池**：国际版（snap-access 国际节点，GeeTest 自动注册）+ 国内码道（OAuth 登录、签到福利），统一调度、熔断、轮换
- 🔁 **智能调度**：配额感知轮换、per-account 并发上限、全局 RPS、健康号 sticky
- 🌐 **协议兼容**：OpenAI `/v1/chat/completions`、`/v1/responses`、Claude Messages 协议
- 🖥️ **Web 管理面板**：账号、代理池、API Key、配额仪表盘
- 🔐 **CN 登录工具链**：`cnlogin` 支持服务器无浏览器扫码登录（ticket 轮询）
- 🎁 **CN 自动化**：每日签到 / 限时福利自动领取、token 自动续期、保活心跳 watchdog
- 🗨️ **会话续接**：CN 渠道按账号维持 `chat_id` 上下文，多轮对话自动续接
- 🌍 **代理池**：账号级 HTTP / SOCKS5 代理绑定与连通性探测

## 🧭 渠道路由（realm 前缀）

模型名支持渠道前缀显式选道（借鉴 [workbuddy2api](https://github.com/ZFXing-lite/workbuddy2api) 的 realm 协议）：

| 模型名写法 | 路由 |
|---|---|
| `cn:snap-chat` / `cn:qwen3-vl-235b` | 只从国内码道账号选号，模型走 CN canonical 管线（不被国际别名改写） |
| `intl:GLM-5.2` / `global:GLM-5.2` | 只从国际账号选号 |
| `GLM-5.2`（无前缀） | 双亲调度：全池统一熔断/轮换/粘性，选中哪边就走哪边协议 |

`/v1/models` 返回国际目录 + `cn:` 前缀的 CN 账号已检出模型目录。CN 渠道总闸 `CN_ENABLED=false` 时 CN 账号整批不进可调集合。

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
