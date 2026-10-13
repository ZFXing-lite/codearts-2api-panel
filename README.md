# CodeArts 2API Panel

一个华为 CodeArts 大模型账号池管理面板，将多个账号的免费模型额度聚合为统一的
**OpenAI / Claude 双协议兼容 API**，自带 Web 管理界面。

## ✨ 特性

- 🔁 **账号池调度**：批量注册/导入账号，自动轮询、配额感知轮换、熔断保护
- 🌐 **双协议兼容**：同时支持 OpenAI (`/v1/chat/completions`)、Responses (`/v1/responses`) 与 Claude Messages 协议
- 🖥️ **Web 管理面板**：账号、代理池、API Key、配额仪表盘一站式管理
- 🔐 **OAuth 自动化**：浏览器自动登录换 token，支持 GeeTest 打码服务接入
- 🌍 **代理池**：账号级 HTTP / SOCKS5 代理绑定，内置连通性探测
- ⚡ **限流与公平调度**：全局 RPS、Per-Key RPM / 并发、上游熔断与重试
- 📦 **单容器部署**：一个 Dockerfile 跑起来

## 🚀 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/ZFXing-lite/codearts-2api-panel.git
cd codearts-2api-panel
cp env.example .env      # 按需修改配置
docker compose up -d --build
```

打开 `http://127.0.0.1:29137` ，使用 `.env` 中的管理员账号登录面板。

### 源码运行

```bash
go build -o codearts ./cmd/server
./codearts
```

要求 Go 1.24+。

## ⚙️ 配置

主要环境变量（详见 `env.example`）：

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `PORT` | 监听端口 | `29137` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | 面板管理员账号 | `admin` / `admin123` |
| `API_KEY` | 对外 API 鉴权 Key（留空则开放） | 空 |
| `DATA_DIR` | 数据目录 | `/data` |
| `REG_AREA_CODE` | 账号注册区域代码 | `OM` |
| `HTTPS_PROXY` | 出站代理（可选） | 空 |

> ⚠️ 生产环境务必修改默认管理员密码。

## 🔌 API 使用

在面板中创建 API Key 后，按 OpenAI 方式调用：

```bash
curl http://127.0.0.1:29137/v1/chat/completions \
  -H "Authorization: Bearer <你的Key>" \
  -H "Content-Type: application/json" \
  -d '{"model": "<模型名>", "messages": [{"role": "user", "content": "hi"}]}'
```

Claude Code 等 Anthropic 客户端：将 `ANTHROPIC_BASE_URL` 指向面板地址即可走 Messages 协议。

## 📁 目录结构

```
cmd/server        程序入口
internal/api      API 网关 / 调度 / 限流 / 熔断
internal/huawei   华为侧接入（OAuth、浏览器自动化、配额）
internal/webui    管理面板前端
internal/store    持久化存储（加密）
internal/proxypool 代理池
```

## 📄 License

MIT

## ⚠️ 免责声明

本项目仅供技术研究与学习交流，请遵守相关平台的服务条款，使用者自负一切责任。
