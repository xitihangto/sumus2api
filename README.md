# sumus2api

把 Sumus（sumus.im）的免费积分账号包装成 **OpenAI 兼容 API**，让其他 agent / 工具零改造接入。

## 它做什么

Sumus 是纯 Web SaaS，只有 Google / Apple / 邮箱登录，没有公开 API。本项目逆向它的私有协议，在本地起一个 OpenAI 兼容网关：

```
你的 agent ──OpenAI SDK──> sumus2api (:7864) ──Sumus 私有协议──> sumus.im
```

- 客户端只写 `/v1/chat/completions` 和 `/v1/models`，现有 SDK 零改造
- 支持流式（SSE）与非流式
- 多账号池轮转、会话粘性（同一对话绑定同一账号）
- 上游是异步轮询模型：发消息立即返回 202，结果靠轮询拉取

## 逆向出的协议

| 用途 | 请求 |
|---|---|
| 身份 | `GET /api/app/me` |
| 工作区 | `GET /api/app/workspaces` |
| 余额 | `GET /api/app/billing/status` → `{"balance":"927970478"}` |
| 模型列表 | `GET /api/app/models` → 21 个模型 |
| 会话列表 | `GET /api/app/sessions?agent_id={aid}` |
| **发消息** | `POST /api/app/sessions/{sid}/messages` → `{"agent_id","content"}` → 202 |
| **取结果** | `GET /api/app/sessions/{sid}/messages?after_seq={N}` |

**鉴权**：纯 cookie，`__Secure-better-auth.session_token`（better-auth 库，HttpOnly + Secure + SameSite=Lax）。不需要 CSRF token。

**统一请求头**：`X-Client-Platform: web` / `X-Client-Version` / `X-Client-Stage: prod` / `X-Client-Locale` / `X-Client-Timezone` / `X-Workspace-Id`

**关键设计**：
- **新建会话**：用 `agent_id` 当 `session_id` 发第一条消息，Sumus 自动建会话并回写真实 session_id
- **多轮上下文**：Sumus 在服务端维护 session 上下文，网关只需把每轮的最后一条 user 消息发过去
- **轮询基线**：发消息前先记录 `newest_seq`，只认这个 seq 之后的新消息，否则会拿到上一轮的旧回复

## 快速开始

### 导出 cookie（两种方式共用）

Sumus 的认证 cookie 是 HttpOnly，JS 读不到。用 ego lite（CDP 通道）导出：

```bash
# 先在 ego lite 里打开 Sumus 并登录，记下 spaceId
ego-browser nodejs -e '
const task = await taskSpace("sumus-export");
const page = task.page("p1");
await page.goto("https://sumus.im/app/sign-in");
console.log("spaceId:", task.spaceId);
'

# 登录完成后，导出 cookie
node scripts/export-cookie.mjs <spaceId> my-account accounts/
```

导出的文件长这样（`workspace_id` / `agent_id` 会在网关启动时自动补齐）：

```json
{
  "session_token": "EJpRDiVWXPl2VzrYlWwUkYVr...",
  "cookie_expires_at": 1794950585
}
```

### 方式 A：Docker 部署（推荐）

```bash
# 1. 准备目录（accounts/ 放上面导出的 cookie）
mkdir -p accounts data

# 2. 起服务
docker compose up -d

# 3. 看日志
docker compose logs -f
```

容器内 `config.json` 的 `api_key` 会被 entrypoint 强制清空（与参考项目一致），网关**不鉴权**运行。如需鉴权，进容器改配置：

```bash
docker compose exec sumus2api vi /app/config.json   # 填入 api_key
docker compose restart
```

> **注意**：compose 里的镜像走加速站 `ghcr.dockerproxy.net/xitihangto/sumus2api:latest`。首次使用前需要先把镜像推上去（见下方"构建与发布镜像"），或改用本地构建：
>
> ```yaml
> services:
>   sumus2api:
>     build: .
>     # 其余不变
> ```

### 方式 B：本地源码运行

前置：Go ≥ 1.22（构建用）。

```bash
# 导出 cookie 到 accounts/ 后启动
go run ./cmd/server -config config.json
```

首次启动自动生成 `config.json`（含随机 `api_key`）并打印：

```
sumus2api 监听 :7864
账号数: 1
API key: b2d04583adc586105158c4faf38c3243fdb8ed6f891aa44e
```

### 构建与发布镜像

本项目用 GitHub Actions 自动构建多架构镜像（amd64 + arm64）并推送到 GHCR：

```bash
# 触发：推送 tag 或手动在 Actions 页面 run
git tag v0.1.0 && git push origin v0.1.0
```

镜像地址：`ghcr.io/xitihangto/sumus2api:latest`
加速拉取：`ghcr.dockerproxy.net/xitihangto/sumus2api:latest`（compose 默认走这个）

本地手动构建（需 docker + buildx）：

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/xitihangto/sumus2api:latest --push .

# 推送后，用加速站拉取：
#   docker pull ghcr.dockerproxy.net/xitihangto/sumus2api:latest
```

### 验证

```bash
KEY=<你的 api_key>

# 健康检查（无需鉴权）
curl http://127.0.0.1:7864/healthz

# 模型列表
curl http://127.0.0.1:7864/v1/models -H "Authorization: Bearer $KEY"

# 流式对话
curl -N http://127.0.0.1:7864/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-6-luna","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 非流式对话
curl http://127.0.0.1:7864/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-6-luna","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

### 5. 接入 OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    api_key="<网关 api_key>",
    base_url="http://127.0.0.1:7864/v1",
)
resp = client.chat.completions.create(
    model="anthropic/claude-sonnet-5.5",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

## 配置说明

`config.json`（首次启动自动生成）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7864` | HTTP 监听地址 |
| `api_key` | 随机生成 | 网关鉴权密钥；空 = 不鉴权（仅限本机调试） |
| `account_dir` | `./accounts` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 状态落盘路径 |
| `upstream.base_url` | `https://sumus.im` | 上游地址（测试时可覆盖） |
| `upstream.client_version` | `53888f788f5e` | 请求头 `X-Client-Version` |
| `upstream.poll_interval_ms` | `1200` | 轮询间隔 |
| `upstream.poll_timeout_seconds` | `300` | 等待 agent 回复的最长时间 |
| `pool.strategy` | `weighted_random` | 选号策略：`weighted_random` / `round_robin` / `sequential_exhaust` |
| `pool.max_in_flight` | `3` | 单账号最大在途请求数，0 = 不限 |
| `session.enabled` | `true` | 会话粘性开关 |
| `session.ttl_minutes` | `30` | 粘性绑定存活时长 |
| `transcript.dir` | `./data/transcripts` | 对话存档目录；**空字符串 = 关闭存档** |
| `transcript.max_bytes_per_file` | `5242880` | 单会话存档体积上限（字节），超出滚动到 `.1` 后缀；0 = 不限 |

### 对话存档

上游 Sumus 不保留可回读的会话历史（结果靠轮询拿，关掉页面就丢）。开启存档后，网关把每轮请求与回复写入 JSONL：

```
data/transcripts/
├── demo-archive-1.jsonl      ← 一个会话一个文件
├── demo-archive-2.jsonl
└── ...
```

每行一条记录：

```json
{"time":"2026-11-11T06:54:16+08:00","conversation":"demo-archive-1","session_id":"b2cf0b9e-...","account":"linqy","model":"openai/gpt-6-luna","stream":false,"duration_ms":34623,"request":[{"role":"user","content":"你好"}],"reply":"你好，存档测试消息收到。","message_id":"01a1c7ab-..."}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `time` | 写入时间（RFC3339，本地时区） |
| `conversation` | 会话标识（`X-Conversation-Id` 或内容哈希） |
| `session_id` | 上游 Sumus 真实会话 ID |
| `account` | 本次实际使用的账号名 |
| `model` / `stream` | 请求参数 |
| `duration_ms` | 从发请求到拿到回复的耗时 |
| `request` / `reply` | 完整请求消息与回复文本 |
| `error` | **非空表示本轮失败**，此时 `reply` 为空 |

要点：

- **异步写入**：存档在独立 goroutine 里跑，不阻塞响应，写失败也不影响对话。
- **成功与失败都记**：401、上游超时等异常同样落盘，便于事后查“当时到底怎么了”。
- **文件名做了消毒**：会话 ID 里的 `/`、空格等会被替换成 `-`，长度截断 80 字符。
- **Docker 下自动持久化**：存档目录在 `data/` 内，已被 compose 挂载，容器重启不丢。
- 面板「对话存档」卡片可查看列表并下载 JSONL。

### 文件与图片输入

网关接受 OpenAI 的多模态 `content` 数组，把文件落到上游 Sumus 后随消息一起发出，模型能真正读到内容（已实测：读出 txt 里的文字、识别出图片颜色）。

支持三种写法：

**1. `input_file` + 内联 base64**（最通用，推荐）

```json
{
  "model": "openai/gpt-6-luna",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "这份文件里的公司代号是什么？"},
      {"type": "input_file", "filename": "note.txt",
       "file_data": "data:text/plain;base64,SGVsbG8gV29ybGQ="}
    ]
  }]
}
```

`file_data` 支持带 `data:<mime>;base64,` 前缀，也支持裸 base64。

**2. `image_url` + base64 data URI**（图片推荐）

```json
{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KG..."}}
```

**3. `image_url` + http/https URL**（网关代下载）

```json
{"type": "image_url", "image_url": {"url": "https://example.com/pic.png"}}
```

几条限制：

- **不支持 `file_id` 引用**。没有实现 `/v1/files` 上传接口，所以客户端若先调 files 接口拿到 `file_id` 再引用，网关会直接报错提示改用 base64 内联，而不是静默丢文件。
- **URL 只允许 `http`/`https`**，`file://` 会被拒绝（防 SSRF 打内网）。
- **URL 下载上限 20 MiB**；超限返回错误。内联 base64 不受此限，但受 HTTP body 大小限制。
- 多个附件可以在一条消息里同时给出，顺序保留。
- 纯字符串 `content`（老式写法）继续可用，行为不变。

**上游协议**（逆向所得，供排查参考）：

```
POST /api/app/uploads
  {"files":[{"name":"note.txt","type":"text/plain","size":11}],
   "target":{"kind":"agent","id":"<agent-id>"},
   "purpose":"attachment"}
  → 返回 upload_url（S3 预签名）、file_url、id

PUT <upload_url>          ← 二进制原文，不能带自定义 header

POST /api/app/sessions/{sid}/messages
  {..., "attachment_ids":["<id>"]}
```

注意第一步的 `size` 必须与实际上传字节数一致，否则上游会拒。

## 可用模型

21 个（实时从 `/api/app/models` 拉取，走 OpenRouter 上游）：

```
anthropic/claude-opus-5.5          anthropic/claude-sonnet-5.5
anthropic/claude-fable-5.2        anthropic/claude-haiku-5.5
x-ai/grok-4.7                     openai/gpt-6-astra
openai/gpt-6.1-sol                openai/gpt-6-luna
openai/gpt-chat-latest            openai/gpt-oss-120b
google/gemini-3.8-flash           google/gemini-4-argon
deepseek/deepseek-v4.1-flash-1017 deepseek/deepseek-v4-pro-0813
~deepseek/deepseek-v4-flash-latest  z-ai/glm-5.3
moonshotai/kimi-k3                moonshotai/kimi-k2.7-code
qwen/qwen3.7-plus                 minimax/minimax-m3
inception/mercury-3
```

## 重要约束

### 积分是一次性的

免费套餐送 **1,000 积分，一次性，不刷新**。这跟订阅制（如 WorkBuddy 的每日签到）完全不同。

实测消耗：短消息约 **4.5 积分/条**，即 1,000 积分 ≈ 220 条短消息。长上下文 / 推理模型消耗更高。

### cookie 7 天过期

`__Secure-better-auth.session_token` 有效期约 **7 天**，**没有 refresh token**。过期后必须重新登录导出。

多账号方案需要每 7 天轮换一次。网关在检测到 401 时会自动把该账号标记为 `disabled`（可在 `/status` 看到），不会静默失败。

### 账号封禁风险

这是非官方网关。Sumus 可能随时收紧反代检测（请求头指纹、通道校验等）。因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

### 仅限自用

本项目仅限管理**你自己**的 Sumus 账号。禁止批量注册小号、对外售卖额度、二次打包分发。

### 附件会消耗额外积分

每个附件都会作为独立输入进入上游，图片按多模态计费，比纯文本贵。积分是一次性的（见上），大量传图会明显加速耗尽。

## 运维端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `/healthz` | 否 | 健康检查，返回 `{"healthy":N,"total":M,"service":"sumus2api"}` |
| `/admin/` | 是 | **账号池管理面板**（内嵌 Web 页面） |
| `/admin/api/accounts` | 是 | 账号列表 + 接入配置（GET）/ 新增账号（POST） |
| `/admin/api/accounts/{name}/enable` | 是 | 启用账号 |
| `/admin/api/accounts/{name}/disable` | 是 | 禁用账号 |
| `/admin/api/accounts/{name}/refresh` | 是 | 刷新余额 |
| `/admin/api/accounts/{name}` | 是 | 删除账号（DELETE，会删磁盘文件） |
| `/admin/api/transcripts` | 是 | 存档文件列表 |
| `/admin/api/transcripts/{file}` | 是 | 下载某个存档文件（JSONL） |
| `/status` | 是 | 账号池详情（JSON，供脚本用） |
| `/v1/models` | 是 | OpenAI 格式模型列表 |
| `/v1/chat/completions` | 是 | OpenAI 格式对话接口（支持多模态附件，见「文件与图片输入」） |

### 账号池管理面板

> **面板地址是 `/admin/`，不是 `/panel/`。**
> 后者是源码里 `go:embed` 的资源目录名（`internal/proxy/panel/`），不是 HTTP 路由。

浏览器打开 `http://<网关地址>:7864/admin/`。

- **网关未设 `api_key`**（Docker 默认）：直接可用。
- **网关设了 `api_key`**：面板页面本身免鉴权，但数据请求会返回 401 并弹出输入框，输入后 key 存入 `sessionStorage`，后续请求以 `?key=` 携带。该方式**仅对 `/admin/api/*` 开放**，`/v1/*` 仍只认 `Authorization` 头。

面板功能：

- **概览卡片**：账号总数、可用数、总积分、即将过期数
- **接入信息**：Base URL / API Key / 选号策略，点击即复制
- **账号表格**：积分、可用状态、在途数、cookie 剩余天数（≤2 天黄色告警，已过期红色）
- **操作**：启用 / 禁用 / 刷新余额 / 删除（带确认）
- **新增账号**：填 `session_token`（可选填名字与过期时间戳），提交后自动预热补齐身份与余额

> 面板与 API 共用同一套鉴权。若网关**未设** `api_key`（Docker 默认），则面板完全开放——任何能访问 7864 端口的人都能增删账号。建议仅在可信网络内使用，或自行在反向代理层加访问控制。

#### 怎么新增一个账号

**第 1 步：拿到 `session_token`**

在 ego lite 里登录 Sumus，然后导出 cookie：

```bash
# 1) 打开 Sumus 并登录，记下输出的 spaceId
ego-browser nodejs -e '
const task = await taskSpace("sumus-export");
const page = task.page("p1");
await page.goto("https://sumus.im/app/sign-in");
console.log("spaceId:", task.spaceId);
'

# 2) 登录完成后导出（把 <spaceId> 换成上一步输出的值）
node scripts/export-cookie.mjs <spaceId> my-account accounts/
```

导出的 `accounts/my-account.json` 里就有 `session_token` 字段。

**第 2 步：在面板里添加**

浏览器打开 `/admin/`，在「账号」卡片顶部的表单填：

| 字段 | 必填 | 说明 |
|---|---|---|
| 账号名 | 否 | 留空则自动用 token 前缀命名；只允许字母、数字、`-`、`_` |
| session_token | **是** | 就是上一步文件里那段 `session_token` 值 |
| 过期时间戳 | 否 | 留空也能用；填了面板才会显示 cookie 剩余天数 |

点「添加账号」，网关会立即写入文件并后台拉取余额/身份。刷新面板即可看到新账号。

**或者直接放文件**（等价做法）：

```bash
# 把导出的凭证文件丢进 accounts/ 目录，重启网关即可被扫描到
cp ~/Downloads/my-account.json ./accounts/
docker compose restart
```

> 账号名（`name`）在网关内部统一为**不带 `.json` 后缀的文件 stem**。面板表格里的「删除」「禁用」等操作都基于它定位文件，所以不要手工给凭证文件起带点号的怪名字。

## 项目结构

```
sumus2api/
├── cmd/server/main.go        # 启动入口：加载配置、预热账号、起 HTTP 服务
├── internal/
│   ├── config/               # 配置加载与默认值
│   ├── account/              # 账号凭证：加载/落盘/在途计数
│   ├── upstream/             # Sumus 私有协议客户端 + 账号池
│   │   ├── client.go         #   HTTP 端点封装 + 附件上传三步
│   │   └── pool.go           #   选号/粘性/轮询对话
│   ├── transcript/           # 对话存档（JSONL，按会话分文件）
│   └── proxy/                # OpenAI 兼容层
│       ├── handler.go        #   路由分发、鉴权、对话接口、多模态解析
│       ├── panel.go          #   账号池管理 API（/admin/api/*）
│       └── panel/            #   内嵌管理面板（embed 进二进制）
│           └── index.html
├── scripts/
│   └── export-cookie.mjs     # 从 ego lite 导出 cookie
├── .github/workflows/
│   └── docker.yml            # 自动构建 amd64+arm64 镜像并推 GHCR
├── Dockerfile                # 多阶段构建，非 root 运行
├── docker-compose.yml        # 一键部署（挂载 accounts/ data/）
├── docker-entrypoint.sh      # 强制清空 api_key + root 降权
├── config.template.json      # 镜像内配置模板（api_key 为空）
├── accounts/                 # 账号凭证目录（.gitignore）
├── data/                     # 状态落盘目录（.gitignore）
└── config.json               # 运行时配置（自动生成，.gitignore）
```

## 与参考项目的差异

参考 [workbuddy2api-panel](https://github.com/linguo26254699/workbuddy2api-panel)（腾讯 CodeBuddy 反代）。核心差异：

| 维度 | WorkBuddy2API | sumus2api |
|---|---|---|
| 鉴权 | OAuth 设备授权（可脚本化） | 浏览器 cookie（7 天过期） |
| 上游形态 | 官方 CLI/桌面端协议 | 纯 Web SaaS 私有协议 |
| 结果获取 | SSE 流推送 | **异步轮询**（无 SSE） |
| 额度 | 每日签到可再生 | **一次性 1,000 积分** |
| 代码量 | 2.6 万行 | ~1,500 行 |
| 反代检测 | 上游已在收紧 | 未知 |

Sumus 的协议更简单（无 SSE 解析、无 token 刷新、无签名），但额度模型更苛刻（一次性、cookie 7 天）。
