# ZafxyrAgent AI · 中文加强版

> 跑在 root 安卓手机上的本地 AI agent。用日常语言说话，它真的照做——开应用、看屏幕、点按钮、敲字、跑 root 命令、管文件、设提醒。

![Platform](https://img.shields.io/badge/platform-Android%2014%2B-3ddc84)
![Root](https://img.shields.io/badge/root-KernelSU%20%7C%20Magisk%20%7C%20APatch-6b7280)
![Daemon](https://img.shields.io/badge/daemon-Go-00ADD8)
![UI](https://img.shields.io/badge/UI-WebView-6b7280)
![License](https://img.shields.io/badge/license-MIT-blue)

**维护者**：[@qingxia5201314](https://github.com/qingxia5201314)
**上游项目**：[zafxyra/ZafxyrAgent](https://github.com/zafxyra/ZafxyrAgent)（作者 [@zafxyra](https://github.com/zafxyra)）
**上游原版文档**：[README.id.md](README.id.md)（印尼语，原样保留）

---

## 这个 fork 改了什么

基于上游 `bb071a2`，做了五件事：

### 1. 新增 6 个界面操作工具（核心）

上游版本只能开关应用和跑 shell——**看不见也点不了屏幕**，所以"打开微信给张三发消息"这种多步任务做不了。这个 fork 补上了这条链路：

| 工具 | 作用 |
|---|---|
| `screen` | 读 `uiautomator dump`，解析成「文字 @(x,y) 可点」的紧凑列表，比截图省一个数量级的 token |
| `tap` | 按 `screen` 返回的坐标点击；`duration_ms>=400` 视为长按 |
| `swipe` | 滚动、翻页、下拉通知栏 |
| `key` | 按键（HOME/BACK/ENTER/POWER…），白名单校验，非白名单也不是纯数字的一律拒绝 |
| `text` | 向输入框输入文字，优先走 ADBKeyboard 通道以支持中文 |
| `screenshot` | 截屏存 PNG 并返回路径 |

坐标一律取节点 bounds 的中心点，模型直接照抄，不用"看图猜位置"。

### 2. 重做移动端界面（对齐 DSH 视觉）

设计 token 直接从 DSH 的 `dsh-client-ui-theme` 和 `dsh-pocket` 的移动端样式提取：底色 `#151517`、卡片层 `#232324` / `#2c2c2e`、强调色 `#7aaaff`。

- 左侧滑出会话抽屉
- 工具调用显示成可展开的卡片
- **Markdown 渲染**：表格、粗体、代码块、列表、链接
- 深色状态栏，打开不再白闪

### 3. 多会话历史

原来只有一条线性记录。现在可以新建、切换、删除、归档，**归档支持一键清空**。会话标题自动取首条消息，旧 `history.json` 启动时自动迁移。

### 4. 模型列表从端点拉取

不再只认配置里写死的那几个。点右下角 ↻ 会向你的 API 端点要真实模型列表（OpenAI 兼容走 `/models`，Gemini 走 `/models?key=`），缓存 10 分钟，拉不到就回退到配置列表。

### 5. 中文化 + 应用图标

- system prompt、界面文案、工具状态标签、APK 内提示全部中文
- 应用图标换成自定义图片，5 个密度齐全
- 默认模型改为 DeepSeek

### 修了 9 个 bug

| 问题 | 后果 |
|---|---|
| 配置文件缺失时 `loadConfig` 提前 return | 所有默认值失效，端口绑到 `0`（随机端口），界面根本连不上 |
| `notify` 对已卸载的 app 重试 30 次 × 4 秒 | 日志刷两分钟 |
| `truncate` 按字节截断 | 中文被劈成半个 UTF-8，界面显示乱码 |
| 配置只从模块目录读 | 刷模块更新会覆盖你的 API Key；且 README 写的路径和代码实际读的不是一个 |
| 改 `base_url` 不切 provider | 配了 DeepSeek 却仍走 Gemini 协议，静默失败 |
| 设置模型不同步 `models` 列表 | 下拉框选不中刚设的模型 |
| 页面加载时自动随机换模型 | 你配的模型被悄悄换掉，用错 key 直接报错 |
| `saveConfig` 写 0644 | 含 API Key 的文件对别的应用可读（改为 0600） |
| 归档唯一的会话后 `activeID` 仍指向它 | 未归档列表空了但"当前会话"还挂着，界面点哪都没反应 |

另有 10 个单元测试覆盖 UI 解析、坐标解析、UTF-8 截断、命令注入防护、配置优先级、会话不变式。

---

## 它是什么

一个 **KernelSU / Magisk / APatch 模块**，在手机里以 `root` 身份跑一个小 daemon（Go 写的）。这个 daemon：

- 在 `http://127.0.0.1:8787` 提供一个轻量 Web 聊天界面（WebView 打开）
- 通过 function calling 把你的手机接到 LLM（OpenAI 兼容 / Gemini）
- 通过**工具**在手机上真的执行动作，而不是只回你一段话

装好并以 root 运行后，它就是一个"能自己动手"的助手。

---

## 环境要求

| 项 | 要求 |
|---|---|
| Android | 14+（minSdk 34） |
| Root | KernelSU、Magisk 或 APatch |
| 架构 | arm64-v8a |
| API Key | 任意 OpenAI 兼容端点（DeepSeek / OpenRouter / 自建 llama.cpp），或 Gemini |

---

## 安装

1. 下载模块 zip（Releases 或自己打包）
2. KernelSU / Magisk / APatch → **模块** → **从存储安装** → 选 zip
3. 重启手机
4. 下拉通知栏，点 **「ZafxyrAgent AI」** 打开聊天界面

安装脚本会自动安装配套 app（`com.zafxyra.agent`）并授予通知权限。

### 自己打包

```bash
./build.sh          # 交叉编译 arm64 到 ai-agent-module/module/bin/agentd
./pack.sh out.zip   # 打包成可刷入的 zip
```

> **Windows 用户注意**：`pack.sh` 会把文本文件统一转成 LF 并自检。
> git 在 Windows 上会把 `.sh` / `module.prop` / `update-binary` 自动转成 CRLF，
> 直接打包刷进去会让安装脚本报 `syntax error: unexpected end of file`。

---

## 配置

**两种方式，任选**：

### A. 界面里改（推荐）

打开聊天界面 → 右上角 **设置** → 填 API 地址、Key、模型 → 保存。

### B. 改文件

```
/data/adb/aiagent/config.json
```

> 配置文件在**用户目录**而不是模块目录，刷模块更新不会覆盖它。
> 模块目录里那份 `/data/adb/modules/aiagent/config.json` 只是出厂模板。

```json
{
  "provider": "openai",
  "api_key": "sk-xxx",
  "base_url": "https://api.deepseek.com/v1",
  "model": "deepseek-chat",
  "max_tokens": 2048,
  "port": 8787,
  "autoopen": true,
  "allow_shell": true
}
```

| 字段 | 说明 |
|---|---|
| `provider` | `openai`（OpenAI 兼容）或 `gemini` |
| `api_key` | 你的 key |
| `base_url` | OpenAI 兼容端点；指向 Google 的会自动按 gemini 处理 |
| `model` | 模型名 |
| `port` | daemon 端口，默认 8787 |
| `autoopen` | 开机后自动打开聊天界面 |
| `allow_shell` | 是否允许 `run_shell`（root 执行任意命令） |

改完重启模块生效（界面里改则即时生效）。

---

## 工具一览（19 个）

**打开类**：`open_app` `open_path` `open_url` `web_search`
**界面类**：`screen` `tap` `swipe` `key` `text` `screenshot`
**系统类**：`run_shell`（root）
**文件类**：`read_file` `write_file` `list_dir` `make_dir` `move_file` `delete_path` `download_url`
**提醒类**：`schedule_message`

### 典型用法

```
看看现在屏幕上有什么
打开设置，把深色模式打开
打开微信，搜索"张三"，进对话
把 Download 里的 apk 都归到一个文件夹
看看电量还剩多少
10 分钟后提醒我吃药
```

---

## 会话管理

左上角 ☰ 打开会话抽屉：

- **新建**：顶栏 ＋ 或抽屉里的「新建会话」
- **切换**：点会话项
- **归档 / 删除**：会话项右侧 ⋯
- **归档箱**：抽屉底部「已归档」，里面可以恢复或删除单条，顶部有**清空全部**

会话存在 `/data/adb/aiagent/sessions.json`，权限 0600。刷模块更新不会影响它。

---

## 已知限制

1. **中文输入**：Android 的 `input text` 只稳定支持 ASCII。装 [ADBKeyboard](https://github.com/senzhk/ADBKeyboard) 后中文才能可靠输入，否则会走降级通道并如实告知你。
2. **FLAG_SECURE 界面**：银行、支付类 App 的截图全黑、UI 树为空，root 也绕不过（系统级保护）。
3. **本地端口**：服务只监听 `127.0.0.1`，同网段访问不到（这是有意的）。
4. **APK 界面文案**：配套 app 里有 4 处印尼语提示（错误提示、通知渠道），尚未中文化。

---

## 安全提醒

- daemon 以 **root** 运行，`run_shell` 等于把 root 交给一个 LLM。不需要就去 config 里把 `allow_shell` 设为 `false`。
- 正在进行的不可逆操作（删文件、卸载应用、发消息、付款），system prompt 要求它先说明再执行——但这只是提示，不是硬约束。**重要数据自己确认**。
- 配置文件权限 0600，别改成 0644。

---

## 致谢

- 原作者 **[@zafxyra](https://github.com/zafxyra)**：整个项目的基础架构、daemon、Web UI、工具框架都来自上游
- 上游 README 原样保留在 [README.id.md](README.id.md)

## License

MIT — 见 [LICENSE](LICENSE)。原始版权归 Zafxyra 所有。
