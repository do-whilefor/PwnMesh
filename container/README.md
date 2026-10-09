# PwnMesh Kali Headless Worker 镜像

以官方 `kalilinux/kali-rolling` 为基础，安装 `kali-linux-headless` 元包及其必需工具依赖，
再安装 pwntools、pymongo、AWS CLI v1、腾讯云 tccli、阿里云 aliyun，以及全局 Playwright CLI 和 Chromium。
`kali-linux-headless` 是 APT 元包，不是 Docker 的 `FROM` 镜像名。

客户端材料使用 `pwn-http` 检查/重放 HTTP 请求，使用 jadx、apktool 和 SQLite 静态分析 APK、Java 归档及本地存储。
完整工具清单、Python 环境、浏览器默认值和运行边界见 [environment.md](environment.md)。

在**仓库根目录**执行。本 Dockerfile 的所有 `COPY` 源路径都以仓库根为构建上下文
（与 `compose.yaml` 中 `worker-image` 的 `context: .` 一致），在 `container/` 目录内构建会找不到源文件。

先构建控制镜像，再构建 Worker 镜像（Worker 末层从 `pwnmesh:dev` 复制 Go 二进制）：

```bash
docker build -t pwnmesh:dev .
docker build -f container/Dockerfile -t pwnmesh-worker:dev .
```

使用其他控制镜像标签时，通过 `--build-arg PWNMESH_IMAGE=pwnmesh:<tag>` 指定。

或通过 compose：

```bash
docker compose --profile images build worker-image
```

控制服务默认只接受 `localhost`、IP 地址及 `--host` 显式绑定的主机名作为 HTTP Host，
拒绝未知域名，避免 DNS rebinding。Compose 已通过 `--allow-host server` 允许内部调度器访问。
反向代理保留外部 Host 时，启动服务需显式添加 `--allow-host pwn.example.com`；
可重复传入或以逗号分隔，填写精确主机名，不带协议、端口或通配符。不会信任 `X-Forwarded-Host`。

模型配置和凭据只保留在 Dispatcher。Worker 镜像和容器不得设置非空的 `ANTHROPIC_*` 环境变量。
主 Agent、子 Agent 和上下文摘要均通过 Dispatcher 调用模型；模型回复经完整检查后交付，文本增量不会实时抵达 Worker。
升级需同时重建控制镜像和 Worker 镜像，不能混用新旧二进制。
升级后，缺少 `pwnmesh.model-boundary=dispatcher-v1` 标签的旧项目容器会被拒绝复用，
不会自动删除：先保全并检查工作区，再迁移到新建容器；不要给旧容器补标签绕过检查。
旧进程、日志或工件中已有的凭据不会因升级消失；确认曾暴露时应轮换凭据。

`run_graph` 命令节点分别保留 `stdout.log` 和 `stderr.log`，回执中的 `output_path`、
`stderr_path` 指向对应完整文件；两份日志均校验 SHA-256，结构化 stdout 不混入诊断信息。
命令图版本已升级到 `mixed-dag-v3`。旧版合流日志无法可靠拆分，旧图恢复和跨图复用会被拒绝，
不会改写原日志或自动重跑命令。升级前应结束活动任务；旧图需先检查已有副作用和证据，再决定后续工作。

### 工具版本

| 构建参数 | 默认值 |
| --- | --- |
| `PWNTOOLS_VERSION` | `4.15.0` |
| `PYMONGO_VERSION` | `4.18.1` |
| `AWSCLI_VERSION` | `1.46.1`（Python 包，CLI v1） |
| `TCCLI_VERSION` | `3.1.173.1` |
| `ALIYUN_CLI_VERSION` | `3.5.1` |

Aliyun 从官方 GitHub Release 下载；升级 `ALIYUN_CLI_VERSION` 时，必须同步
`ALIYUN_CLI_SHA256`，使用对应版本、Linux amd64 发布包的校验值。
默认值来源于 [v3.5.1 的 SHASUMS256.txt](https://github.com/aliyun/aliyun-cli/releases/download/v3.5.1/SHASUMS256.txt)。
可通过 `--build-arg <参数名>=<版本>` 覆盖这些默认值，修改后重新构建并运行自检。

Node.js 和 npm 统一由 Kali APT 提供，提供 `node`、`npm`、`npx`；自检要求 Node.js 至少为 20。
Playwright 按 `@playwright/cli@latest` 全局安装，构建时执行 `playwright-cli install`
预装匹配的 Chromium。Docker 缓存可能复用先前解析的版本；需要重新获取 `latest` 时，
使用 `docker build --no-cache -f container/Dockerfile -t pwnmesh-worker:dev .`，并重新运行下方自检。

### apt 镜像源

镜像默认使用基础镜像中的 Kali 官方分发源。分发节点不可用时，可以固定使用
Kali 官方 HTTPS 下载站；也支持中科大 USTC 等镜像源。缺省留空表示行为不变：

```bash
docker build -f container/Dockerfile \
  --build-arg KALI_MIRROR=https://kali.download/kali \
  -t pwnmesh-worker:dev .
```

该参数只改写 `sources.list.d/kali.sources` 的 `URIs` 字段，套件、组件与签名配置保持不变。

支持 `http://` 和 `https://`。固定的裸 Kali 镜像缺少 CA 证书；Dockerfile 在首次
APT 请求前从控制镜像复制 CA 证书包，并用 `Acquire::https::CaInfo` 显式指定路径，
再正常安装 Kali 的 `ca-certificates`。
因此 HTTPS 源不再依赖先用 HTTP 安装证书，也不会关闭 TLS 证书校验或 APT 签名校验。
自定义 `PWNMESH_IMAGE` 必须同时提供 PwnMesh 二进制和 `/etc/ssl/certs/ca-certificates.crt`。

APT 索引下载失败会使构建失败，避免把使用旧索引的警告误判为成功；临时下载错误最多重试两次。

### 离线验证与使用

`check-worker.sh` 已随镜像分发到 `/usr/local/share/pwnmesh/`。自检检查 headless 元包和新增工具，
实际执行回环 ping、文本与 YAML 处理、pwntools 汇编和常量求值、
BSON 编解码、云 CLI 版本命令、由 groff-base 渲染的 AWS CLI 帮助，以及 Playwright CLI 打开回环地址页面并验证 JavaScript 运行结果；
还会现场生成无外部依赖的 JAR/多 DEX APK，验证代码反编译、Manifest 权限/导出组件/深链/备份声明、网络安全资源和 assets 的解码，以及原 APK 字节不变；
另验证 SQLite 只读查询和 `pwn-http` 解析/重放回环服务的 HAR 并校验证据。
无需外网或云凭据。

```bash
# 运行镜像内置的环境与运行结构检查
docker run --rm --pull never --network none --init \
  --entrypoint /usr/local/share/pwnmesh/check-worker.sh pwnmesh-worker:dev

# 验证镜像自身 ENTRYPOINT/CMD（等价于 pwnmesh worker --help）
docker run --rm --pull never --network none pwnmesh-worker:dev
```

### 客户端接口与静态分析

在项目中上传抓包文件（HAR 或原始 HTTP 请求）、APK、源码包、配置或本地数据库。给任务描述授权范围、目标账号角色和要验证的业务行为；使用上传结果给出的实际 Worker 路径。上传不会自动执行或解压文件。

在「创建项目」中选择测试材料，文件全部上传成功后才启动任务；上传失败可原地重试，已创建项目保持暂停。
现有项目通过「补充」添加文件，也可以不填写文字。本次选择的文件与说明一起保存，全部接收成功后才供新任务使用；提交被拒绝时整批不保存，重试会重新发送本批文件。源码目录请先打包为 ZIP/TAR；配置、脚本、数据库可以直接上传。
每个文件最大 256 MiB，每个项目最多 32 个文件、合计 512 MiB。同名同内容重试会去重，变更内容保留为新文件。
文件原件与项目数据一起保存，重启项目保留，删除项目一并删除；上传内容不属于凭据保险库。
新增材料供后续新任务使用，已经运行的任务仍保留原输入。Worker 中路径为 `/workspace/.pwnmesh/inputs/<id>/<文件名>`；修改或解压前先复制到工作目录。

API 也可使用 `POST /projects/{pid}/inputs` 上传单个 multipart `file` 字段，`GET /projects/{pid}/inputs` 获取元数据清单，`GET /projects/{pid}/inputs/{id}` 下载原件。创建时传 `start_paused:true` 可以在上传完成后通过项目状态接口开始调度。

活动项目的成组补充使用 `POST /projects/{pid}/inputs/batch`：multipart 中包含一个或多个 `file` 字段和可选的 `content` 文字字段（最多 32768 个字符），成功返回文件元数据数组。文件、说明和对应状态变更在同一事务中保存。单文件接口仍会立即发布该文件；上传与下载的大文件传输预算均为 5 分钟。

下面假设已把材料放在 `/workspace/inputs/`；输出目录应是新目录：

```bash
# 先离线核对捕获到的请求和可重放状态，再选择有授权的请求。
mkdir -p /workspace/evidence /workspace/analysis
pwn-http inspect /workspace/inputs/client.har
pwn-http replay /workspace/inputs/client.har --index 1 --output /workspace/evidence/role-a

# 单独准备测试身份 B 的 JSON 头覆盖文件，例如 {"Authorization":"Bearer ...","Cookie":null}。
# 原始请求、头覆盖文件及证据可能包含敏感数据，不要提交到 Git。
pwn-http replay /workspace/inputs/client.har --index 1 \
  --headers /workspace/inputs/role-b-headers.json --output /workspace/evidence/role-b

# 原始 HTTP 请求的 request-target 为相对路径时，显式提供匹配的源站。
pwn-http inspect /workspace/inputs/request.http --base-url https://api.example.invalid

# 解码 APK，分析代码、Manifest、资源及本地存储。
file /workspace/inputs/client.apk
jadx -d /workspace/analysis/java /workspace/inputs/client.apk
apktool d /workspace/inputs/client.apk -o /workspace/analysis/apk
rg -n 'https?://|android:exported|allowBackup|debuggable|networkSecurityConfig|cleartextTrafficPermitted' /workspace/analysis
sqlite3 -readonly /workspace/inputs/client.db '.schema'
```

`pwn-http` 只重放指定的一条请求，不自动重试或跟随重定向；HTTP 4xx/5xx 仍记录响应证据。
`--headers` 使用 JSON 字符串值覆盖头，用 null 删除头；也可用 `--remove-header <名称>`。
请求头中的敏感值和 URL 查询值会脱敏，但请求/响应 body 保留原始字节供核验，不能将证据目录视为已经完全脱敏。
比较两个身份的返回内容和状态只能提供测试证据，权限问题仍需按业务权限和实际影响确认。

抓包需由测试设备和已有代理工具完成后导入；本次不提供手机原生界面操作。
受设备签名、客户端证书或会话绑定保护的接口需要原设备参与或补充测试条件。
APK 中的导出声明、深链和网络配置必须结合组件权限、代码校验、资源限定符和目标 Android 版本判断；关键词命中本身不是漏洞证明。
多 DEX 应检查全部反编译结果；拆分 APK 需要完整 split 集，单个 base APK 不代表完整应用。
静态自检不覆盖签名真实性或 Android 运行时行为；加固包、iOS 二进制或本地原生库应按材料补充对应工具与运行环境。
