# bundle-skill-dev.md — Agent Skill 发布 + 注册系统

> Agent 侧主导的设计文档。所有 wire protocol 决策以 agent 当前能消费的为准；dock / polar-hosts 端在配套 PR 里贴齐。

## 1. 现状（2026-05-25 落地实情）

代码翻过一遍，今天 agent 端的 skill 系统是这样的：

| 项 | 实际 |
|---|---|
| Skill 定义 | Go interface（Kind / Version / Capabilities / Validate / Start），编译进 binary |
| 已注册的 kind | `coder` · `shell` · `proxy` · `wg` · `kdp` · `vnc` · `mcp` · `bundle`（8 种，全部 init 时 `Register()`） |
| 外部 skill 路径 | **只有 `bundle`** — agent 下载 `.skill` ZIP → 校验 SHA256 → 解压到 `~/.polar/bundles/<name>/<version>/` → 跑 `entrypoint`（可选 venv） |
| `skill.advertise` 帧 | 60s 一次 WS push：`{kind:"skill.advertise", skills:[{kind,version,capabilities},...]}` |
| Dock 路由 | agent_hub.go 接到 → `forwardSkillAdvertiseToPolarHosts()` → POST `/internal/v1/hosts/touch`（loopback） |
| polar-hosts 持久化 | `host_skills(host_id, kind, name, config_json, enabled, auto_start, ...)` — UNIQUE on `(host_id, kind)` |
| Skill 身份 | 仅 `Kind` 字符串。**没有 stable UUID，version 不参与索引，同 kind 不同 version 在 DB 里被合并** |
| 市场 / 目录 | 不存在。dock 把 bundle 的 `download_url + sha256 + entrypoint` 硬编码在 bot 编辑表单里 |
| 签名 | 不存在 |
| 安装/卸载 wire frame | 不存在。bundle 通过 bot 配置 + 重启间接触发 |

`KindBundle` 是已经存在的、可用的下载-执行通道。本文档不重新发明它——而是**在它周围补齐 publish/register 的缺失能力**。

## 2. 设计目标

围绕 4 个能力：

1. **Bundle 是可独立发布的产物**：第三方按规范打包 `.skill` ZIP，任何 agent 装上就能运行
2. **Catalog 是首选答案**：任何 agent / 任何工作区可以问"我能装哪些 skill"，而不是靠 bot 表单硬编码
3. **Agent 是 source of truth**：dock 可以请求"请装 X"，但**装不装、装在哪、跑不跑由 agent 决定**——dock 拿到的是 advertise 上报的事实
4. **可逆**：每一步都有显式 uninstall。装错了不需要操作员 ssh 进 agent 删目录

非目标（明确不做）：
- 沙箱化（bundle 跑在 agent 进程权限下，运维责任）
- 跨架构 binary 分发（bundle 默认 python/shell；纯 binary 走单独 packaging，不在 P0-P3 范围）
- 远程依赖管理（venv 内部 pip install 就够，不做更复杂的）

## 3. 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│  发布者 (publisher)                                         │
│  按 §5 spec 打包 .skill ZIP + manifest                      │
└────────────────────┬────────────────────────────────────────┘
                     │ POST /api/skill-catalog  (admin)
                     ▼
┌─────────────────────────────────────────────────────────────┐
│  polar-hosts  ::  skill_catalog 表                          │
│  (publisher, kind, version) UNIQUE → sha256 + download_url  │
└────────────────────┬────────────────────────────────────────┘
                     │  GET /api/skill-catalog
                     │  POST /api/hosts/:id/skills/install   ← 操作员触发
                     ▼
┌─────────────────────────────────────────────────────────────┐
│  dock                                                       │
│  发 WS frame:  {kind:"skill.install", id, sha256, url, ...} │
└────────────────────┬────────────────────────────────────────┘
                     │  WS
                     ▼
┌─────────────────────────────────────────────────────────────┐
│  agent                                                      │
│  下载 → 校验 → 解压 → manifest 校验 → 注册到 runtime registry │
│  下一次 skill.advertise tick 反馈安装成功 + 新的 capabilities │
└─────────────────────────────────────────────────────────────┘
```

**关键不变量**：agent 收到 `skill.install` 不等于 skill 就活了。agent 装完会在**下一次 `skill.advertise` 60s tick**里报告 `installed=true`。dock 以 advertise 为准——不靠"我以为我装了"。

## 4. Skill 身份

```
<publisher>/<kind>@<version>
```

- **publisher** — 反向域名风格 (`com.networkextension.kdp`)。没有 publisher = 平台内置（`platform/shell@1.0.0`）
- **kind** — 小写短串，`.` 分隔子类型 (`shell`, `mcp.lldb`, `vnc.macos`)
- **version** — semver；agent 不解析，只做字面相等比较

**SHA256(bundle.zip)** 是内容指纹。`(publisher, kind, version)` 是引用名。两者都必须匹配才允许安装。

`host_skills` 表得加一列存 `publisher@version`（详见 P0b 迁移），让一个 host 可以同时持有同 kind 不同 publisher 的 skill。但**同 (publisher, kind) 只能有一个 version 在 host 上 active**——升级 = 卸 + 装。

## 5. Bundle 文件格式

```
my-skill.skill/                   # 实际是 zip 文件，扩展名 .skill
├── manifest.yaml                 # 必需 — 见下
├── README.md                     # 推荐
├── scripts/
│   └── run.py                    # entrypoint（manifest.entrypoint 指向它）
├── requirements.txt              # 可选 — pip install 到 venv
├── data/                         # 可选 — 静态资源
└── LICENSE                       # 推荐
```

### 5.1 manifest.yaml schema

```yaml
# 必需字段
publisher: com.networkextension.kdp
kind: kdp
version: 0.3.0
entrypoint: scripts/run.py

# 推荐字段
display_name: Apple KDP Recovery Helper
description: |
  Boot-time iOS device detection + KDP debug bring-up.
license: Apache-2.0
homepage: https://github.com/networkextension/polar-kdp

# 运行时声明（agent 用这个决定怎么起）
runtime:
  kind: python              # python | shell | binary
  python_version: ">=3.10"  # 仅 python
  venv: true                # 仅 python；agent 在装时 pip install requirements.txt
  args: []                  # 追加到 entrypoint 后面
  env:                      # bundle 自带的 env；操作员配置在 host_skills.config_json 里覆盖
    KDP_LOG_LEVEL: info

# Capabilities — 这个 skill 能做什么（dock UI + bot 调度用）
capabilities:
  tools:
    - name: detect_recovery_device
      description: List iOS devices in recovery / DFU
    - name: bring_up_kdp
      description: Start a KDP debug session against a connected device

# 需要的本机权限（agent 装时检查，缺权限拒装）
requires:
  - usb           # 访问 USB 设备
  - python: ">=3.10"
  # 可选: macos_app_sandbox_exempt, root, network_admin

# 安装 hint（可选 — agent 默认行为已经够好）
install:
  size_max_mb: 50            # 拒装超过 N MB 的 bundle（保护 agent 磁盘）
  timeout_seconds: 120       # 解压 + venv setup 超时
```

### 5.2 SHA256

- `sha256(.skill ZIP)` 是 catalog 里的 `sha256` 字段
- agent 下完包**第一件事就是 sha 校验**——不通过直接拒装、记日志、丢回 `skill.install.result {status: "sha_mismatch"}`
- manifest 内部的 file integrity 不算（zip 内的文件已被 zip 的 CRC + 外层 sha 双层保护）

## 6. Wire protocol（agent ↔ dock）

### 6.1 已有

```jsonc
// agent → dock，每 60s
{
  "kind": "skill.advertise",
  "skills": [
    { "kind": "shell", "version": "1.0.0", "capabilities": {...} },
    { "kind": "kdp",   "version": "0.3.0", "capabilities": {...} }
  ]
}
```

### 6.2 新增 — install 

```jsonc
// dock → agent
{
  "kind": "skill.install",
  "install_id": "ins_2026-05-25-a1b2",  // 幂等 key；agent 用来去重
  "publisher": "com.networkextension.kdp",
  "skill_kind": "kdp",
  "version": "0.3.0",
  "sha256": "abc123...",
  "download_url": "https://zen.4950.store/api/skill-catalog/ins_.../download",
  "size_bytes": 4192304,
  "manifest_preview": { ... } // 可选，dock 提前给 agent 看 manifest，agent 可以在装之前拒（如 requires.usb 不满足）
}

// agent → dock
{
  "kind": "skill.install.result",
  "install_id": "ins_2026-05-25-a1b2",
  "status": "ok" | "sha_mismatch" | "manifest_invalid" | "requires_unmet" | "download_failed" | "venv_failed" | "disk_full" | "timeout",
  "installed_path": "~/.polar/bundles/com.networkextension.kdp/kdp/0.3.0/",
  "error": "可选，人类可读的错误描述",
  "duration_ms": 4321
}
```

### 6.3 新增 — uninstall

```jsonc
// dock → agent
{
  "kind": "skill.uninstall",
  "install_id": "uni_2026-05-25-c3d4",
  "publisher": "com.networkextension.kdp",
  "skill_kind": "kdp",
  "version": "0.3.0",
  "force": false               // 默认 false → 如果该 skill 当前有 active run，拒绝；true 则先 stop
}

// agent → dock
{
  "kind": "skill.uninstall.result",
  "install_id": "uni_2026-05-25-c3d4",
  "status": "ok" | "active_runs" | "not_installed" | "fs_error",
  "removed_runs": 0,
  "error": "..."
}
```

### 6.4 advertise 升级

P0a 起，advertise 帧每条 skill 必须带：

```jsonc
{
  "kind": "kdp",
  "version": "0.3.0",
  "publisher": "com.networkextension.kdp",   // 新增
  "install_id": "ins_2026-05-25-a1b2",       // 新增（可空，平台内置 skill 没有）
  "source": "builtin" | "bundle",            // 新增 — bundle 表示是 install 装的
  "capabilities": {...}
}
```

## 7. 分期 (Phases)

每个 phase 一个独立 PR，可独立 merge / rollback。phase 内部按 `Pna-1 / Pna-2 / ...` 分子 PR（参考 wg-mac phase 2 的拆法）。

---

### **P0** — 现状盘点 + 文档化 (本 PR)

写 `bundle-skill-dev.md`（本文档）。**不写任何代码**——只是把已有 KindBundle、`skill.advertise`、`host_skills` 三个事实串成一个完整的"系统"叙事，为后面的 phase 提供共同语言。

**验收**：本文档进 main，所有 phase PR 的描述都引用本文档对应小节。

---

### **P0a** — Bundle manifest 规范化（agent 端）

**范围**：把今天散在 `BundleConfig{ Entrypoint, Args, Env, ... }` 的字段拉到一个独立 `manifest.yaml` 文件里。

**改动**：
- `cmd/polar-agent/skills/bundle_manifest.go` — 新文件，解析 + 验证 manifest schema
- `cmd/polar-agent/skills/bundle.go` — 装包时如果 ZIP 里有 `manifest.yaml`，优先用 manifest 内容覆盖 `BundleConfig`
- `doc/bundle-format.md` — 把 §5 抽出来作为独立 spec
- `scripts/example-skill/` — 一个最小可跑的 reference bundle（python echo skill，<1 KB）

**向后兼容**：dock 今天传的 `BundleConfig` 继续工作。manifest.yaml 缺失时 agent 行为不变。`requires.usb` 等新字段在 manifest 缺失时默认 unrestricted。

**验收**：example-skill 打包 → manifest 校验通过 → 装上 → advertise 报告 `installed=true, source="bundle"`。

---

### **P0b** — Catalog 表 + REST（polar-hosts 端）

**范围**：把"可装的 skill"从 dock 硬编码挪到 polar-hosts DB。

**改动**：
- `polar-hosts/scripts/migrate/skill-catalog.sql` — 建表：

```sql
CREATE TABLE skill_catalog (
  id            text PRIMARY KEY,        -- "scat_<random>"
  publisher     text NOT NULL,
  skill_kind    text NOT NULL,
  version       text NOT NULL,
  sha256        text NOT NULL,
  download_url  text NOT NULL,           -- 绝对 URL；P3 起可以是 R2 签名 URL
  size_bytes    bigint NOT NULL,
  manifest_json jsonb NOT NULL,          -- 解析过的 manifest.yaml
  display_name  text,
  description   text,
  license       text,
  homepage      text,
  publisher_pubkey text,                 -- P2 起填，P0b 留空
  is_platform   boolean NOT NULL DEFAULT false,
  workspace_id  text,                    -- platform = NULL；workspace skill = 指定
  created_by    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  retired_at    timestamptz,
  UNIQUE (publisher, skill_kind, version)
);

CREATE INDEX idx_skill_catalog_lookup ON skill_catalog (publisher, skill_kind, version) WHERE retired_at IS NULL;
CREATE INDEX idx_skill_catalog_workspace ON skill_catalog (workspace_id, retired_at);
```

- `polar-hosts/internal/hosts/skill_catalog_store.go` — typed CRUD
- `polar-hosts/internal/hosts/skill_catalog_handlers.go` — REST:

```
GET    /api/skill-catalog              list
POST   /api/skill-catalog              register a new bundle (admin)
GET    /api/skill-catalog/:id          detail
POST   /api/skill-catalog/:id/retire   soft delete
GET    /api/skill-catalog/:id/download (proxy to download_url OR serve from storage)
```

- `polar-hosts/scripts/upload-bundle.sh` — dev/CI 工具：本地 `.skill` 包 → upload to R2 → POST catalog row

**还没做**：UI（P1b），WS install frame（P1a）。catalog 暂时通过 REST + curl 操作。

**验收**：管理员 curl 上传一个 `.skill` → POST `/api/skill-catalog` 成功 → `GET /api/skill-catalog` 返回它。

---

### **P1a** — agent 实现 skill.install / skill.uninstall

**范围**：agent 真正能 react 到 dock-initiated install。

**改动**：
- `cmd/polar-agent/loop.go` — 在 WS read loop 里加 dispatcher，识别 `kind: "skill.install"` / `"skill.uninstall"`
- `cmd/polar-agent/skills/installer.go` — 新文件：
  - `Install(req InstallRequest) InstallResult` — 下载 → SHA 校验 → 解压 → manifest 校验 → requires 检查 → venv setup → `Register()` 到 runtime registry → 触发一次 `skill.advertise`
  - `Uninstall(req UninstallRequest) UninstallResult` — 检查 active runs → 从 registry 移除 → 删除磁盘
  - 幂等：`install_id` 缓存在 `~/.polar/installs.json`，重复请求直接返回上次结果
- `cmd/polar-agent/skills/installer_test.go` — table-driven: sha mismatch / manifest invalid / requires unmet / venv failure / disk full

**dock 侧改动（最小）**：
- `polar-dock/internal/app/dock/agent_hub.go` — 一个新方法 `SendSkillInstall(hostID string, req)` 把 frame 推到对应 agent WS
- 暂时通过 REST 触发：`POST /api/hosts/:id/skills/install` body = catalog id

**验收**：管理员调 REST → dock 推 frame → agent 装上 → 下次 advertise 报告 → polar-hosts.host_skills 出现新行。

---

### **P1b** — Marketplace UI

**范围**：dock 加 `/skills.html`（参考 `/llm.html` 模式）。

**改动**：
- `polar-dock/ui/public/skills.html` — 新页：左侧 catalog 列表，右侧选中的 skill 详情 + "Install on host X" 按钮
- `polar-dock/ui/src/skills.ts` — 调 polar-hosts 的 `/api/skill-catalog`，调 dock 的 `/api/hosts/:id/skills/install`
- sidebar nav 加 "Skills" 入口
- 显示安装态：interrogate `host_skills` 表（已存在的 GET 路径），每条 catalog 行配对 N 个 host 显示 ✓ / 未装 / 安装失败

**验收**：浏览器装一个 example-skill 到 .57 host → 30s 内 UI 翻新成 "✓ 已安装"。

---

### **P2** — Signing

**范围**：bundle 可以被 publisher 签名，agent 在 install 前验证签名。

**改动**：
- `cmd/polar-agent/skills/installer.go` — 加 signature verification（Ed25519，公钥在 manifest 或 catalog 行里）
- `polar-hosts/skill_catalog.publisher_pubkey` 列起作用
- 配置开关：`POLAR_AGENT_REQUIRE_SIGNED=true` → 拒装所有未签名 bundle
- `polar-agent/doc/signing.md` — publisher 怎么签包的说明（`openssl + skill-sign.sh`）

**验收**：未签名 bundle 在 `require_signed=true` 时被 agent 拒装，错误码 `signature_missing` 经 skill.install.result 回传。

---

### **P3** — Workspace ownership + 平台 vs 私有 bundle

**范围**：catalog 的 `workspace_id` / `is_platform` 字段在 UI/REST 真正起作用。

**改动**：
- 列 catalog 时按 workspace 过滤（参考 `/llm-configs/marketplace` 的写法）
- 平台 bundle (`is_platform=true`) 对所有 workspace 可见
- workspace bundle 仅本 workspace 可见
- 装 skill 时 agent 检查 host 所属 workspace ↔ bundle workspace 是否匹配；不匹配 dock 拒推

**验收**：B 工作区上传的 bundle 在 A 工作区不可见、装不上。

---

### **P4** — Auto-update（推迟到 P2/P3 baked 之后）

可选。同 publisher+kind 出现更高 version 时，agent 主动询问 dock 是否更新，操作员决定。

---

## 8. 文件清单

按 phase 列要创建/改动的文件，方便每个 PR 的边界。

### P0a — polar-agent
- ✏ `cmd/polar-agent/skills/bundle.go` — 解析 manifest.yaml
- ➕ `cmd/polar-agent/skills/bundle_manifest.go`
- ➕ `cmd/polar-agent/skills/bundle_manifest_test.go`
- ➕ `doc/bundle-format.md`
- ➕ `scripts/example-skill/` (包含 manifest.yaml + scripts/run.py)

### P0b — polar-hosts
- ➕ `internal/hosts/skill_catalog_store.go`
- ➕ `internal/hosts/skill_catalog_handlers.go`
- ➕ `internal/hosts/skill_catalog_store_test.go`
- ➕ `scripts/migrate/skill-catalog.sql`
- ➕ `scripts/upload-bundle.sh`
- ✏ `internal/hosts/plugin.go` — 注册新 routes

### P1a — polar-agent + polar-dock
- ➕ `polar-agent/cmd/polar-agent/skills/installer.go`
- ➕ `polar-agent/cmd/polar-agent/skills/installer_test.go`
- ✏ `polar-agent/cmd/polar-agent/loop.go` — frame dispatcher
- ✏ `polar-dock/internal/app/dock/agent_hub.go` — `SendSkillInstall()`
- ➕ `polar-dock/internal/app/dock/skill_install_handlers.go` — REST endpoint
- ✏ `polar-dock/internal/app/dock/app.go` — route registration
- ➕ host_skills schema migration: 加 `publisher` + `install_id` + `source` 列

### P1b — polar-dock
- ➕ `ui/public/skills.html`
- ➕ `ui/src/skills.ts`
- ✏ 14 个 sidebar 模板：加 "Skills" 入口

## 9. 失败模式 + 边界

| 场景 | agent 反馈 | 操作员看到 |
|---|---|---|
| download_url 404 | `skill.install.result {status: download_failed}` | UI 上显示红色 ❌ + HTTP 状态码 |
| SHA256 不匹配 | `{status: sha_mismatch, sha_observed: "..."}` | UI 红色，"该 bundle 可能已被篡改" |
| manifest 缺字段 / yaml 格式坏 | `{status: manifest_invalid, error: "..."}` | UI 红色 + manifest 解析错误行号 |
| `requires.usb` 但 host 不是 macOS | `{status: requires_unmet, missing: ["usb"]}` | UI 显示 "该 host 不满足 skill 要求" |
| venv setup 超时 | `{status: timeout, phase: "venv_install"}` | UI 红色，提示加大 install.timeout_seconds |
| Bundle 超 size_max_mb | agent 拒装，不下载 | UI 显示 "bundle 超过 N MB 上限" |
| Active runs 阻止 uninstall | `{status: active_runs, run_ids: [...]}` | UI 提示 "请先停止运行中的 run" |

**Agent 磁盘保护**：所有 bundle 加起来超过 `POLAR_AGENT_BUNDLE_DIR_MAX_MB` (默认 2048) 时拒装新 bundle。

## 10. 与现有系统的关系

| 已存在 | 本设计 | 关系 |
|---|---|---|
| `host_skills` 表 | 加 `publisher` / `install_id` / `source` 列 | 兼容；旧行 source='builtin'，publisher='' |
| `skill.advertise` 帧 | 加 `publisher` / `install_id` / `source` 字段 | 旧 agent 不报新字段，dock fallback 旧逻辑 |
| `KindBundle` | 不动 | manifest.yaml 只是给它的 BundleConfig 多一种来源 |
| MCP adapter registry | 不直接相关 | 未来 MCP adapter 也可以当 bundle 发布（manifest.kind="mcp.xxx"），但 P0-P3 不强求 |
| `/llm.html` 市场 | 参考模板 | `/skills.html` 复用 byId() + 表格渲染范式 |
| polar-sdk | 不动 | polar-hosts 的 catalog REST 调 dock /internal/v1/auth/verify 走现有 SDK |

## 11. 不在本设计范围

- **沙箱化**：bundle 当前以 agent 进程权限跑。Phase ≥ P5。
- **多架构 binary 分发**：bundle 默认 python/shell。要分发原生 binary 时（如 KDP C 库）走单独的 "binary skill" 通道，那是另一个设计。
- **Bundle 间依赖**：A skill require B skill。手动管理；P0-P4 不解决。
- **Operator 配置 vs bundle 默认配置的合并语义**：留到 P1c。当前 `host_skills.config_json` 直接覆盖 manifest defaults。

---

## 附 A：reference example-skill 长这样

```
example-skill/
├── manifest.yaml
└── scripts/
    └── run.py
```

```yaml
# manifest.yaml
publisher: com.networkextension.example
kind: echo
version: 0.1.0
entrypoint: scripts/run.py
display_name: Echo
description: Read lines from stdin, write them back to stdout. Reference skill.
runtime:
  kind: python
  python_version: ">=3.10"
  venv: false
capabilities:
  tools:
    - name: echo
      description: Identity transform on text
requires: []
```

```python
#!/usr/bin/env python3
# scripts/run.py
import sys
for line in sys.stdin:
    sys.stdout.write(line)
    sys.stdout.flush()
```

打包：
```
cd example-skill && zip -r ../com.networkextension.example_echo_0.1.0.skill . -x "*.DS_Store"
sha256sum ../com.networkextension.example_echo_0.1.0.skill
```

注册：
```
curl -X POST https://zen.4950.store/api/skill-catalog \
  -H "Content-Type: application/json" \
  -d '{
    "publisher": "com.networkextension.example",
    "skill_kind": "echo",
    "version": "0.1.0",
    "sha256": "...",
    "download_url": "https://r2.example.com/.../com.networkextension.example_echo_0.1.0.skill",
    "size_bytes": 1024,
    "is_platform": true
  }'
```

装到某个 host：
```
curl -X POST https://zen.4950.store/api/hosts/h_xxx/skills/install \
  -H "Content-Type: application/json" \
  -d '{"catalog_id":"scat_yyy"}'
```

P1b 起这一切都在 UI 里完成。
