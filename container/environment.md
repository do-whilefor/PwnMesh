# PwnMesh Worker 环境

- 基于官方 Kali rolling 镜像（`linux/amd64`），安装 `kali-linux-headless` 元包及其必需工具依赖，使用 Go `pwnmesh worker` 执行 Agent Loop。
- `/workspace` 是同一项目共享的工作目录；`/home/kali/workspace` 指向同一目录，且已初始化为 git 仓库。
- `/workspace/.pwnmesh/runs/<run-id>` 保存单次执行的任务、会话和工具输出。
- 默认用户为 root，以兼容 Dispatcher 写入的私有任务文件。保留 `kali` 用户和免密码 sudo，便于手动使用。
- 时区为 `Asia/Shanghai`，Python 输出不缓冲。
- 首次 APT 请求前从控制镜像提供 CA 证书，支持 HTTPS 镜像源；构建完成后由 Kali 的 `ca-certificates` 管理证书。

## 基础工具

bash、curl、wget、ripgrep (`rg`)、fd (`fdfind`)、Python 3.13、pip、venv、jq、git、coreutils、procps (`ps`)、iproute2 (`ip`)、dnsutils (`dig`)、zip/unzip、sudo，以及 CA 证书和时区数据。另含 binutils、cpp、C/C++ 构建工具和 Python 3.13 开发依赖，支持 pwntools 的本机汇编及 Python 包源码安装。

另外显式安装 bsdextrautils（`column`、`hexdump`）、Node.js/npm、iputils-ping（`ping`）、sshpass、ncat、rlwrap、yq、krb5-user（`kinit`、`klist`）、adb。`yq` 是 Kali 提供的 jq 风格 Python 实现，可用 `yq -r '.name' file.yaml` 读取字段。

headless 工具集包含 nmap、sqlmap 等命令及其随包数据；APT 不额外安装 Recommends。软件包安装期间不会自动启动服务。容器入口仍为 PwnMesh，不运行 systemd；任务需要时可直接启动相应服务进程。

Nmap 去除了 Kali 软件包附带的文件 capabilities，避免 `CAP_NET_ADMIN` 超出 Docker 默认权限导致启动失败；默认 root 进程沿用容器已有权限。

## 客户端接口与本地材料

项目可以上传 HAR、原始 HTTP 请求、APK、源码压缩包、配置和数据库等二进制文件。上传结果中的工作区路径就是 Worker 的输入路径；上传本身不解压或执行文件。使用任务要求说明授权接口范围、分析目标和不同测试账号的身份。

`pwn-http inspect <文件>` 离线读取 HAR 或原始 HTTP 请求，输出请求索引、方法、脱敏 URL、请求头名称和 body 大小。`pwn-http replay <文件> --index 1 --output <新目录>` 单次发送选中的完整请求并保存请求/响应证据；不会自动跟随跳转或重试。原始请求使用相对路径时需指定 `--base-url <源站>`。HTTP 错误状态同样保存为证据；缺失或无法完整还原的 body 会拒绝重放。

用 `--headers <JSON文件>` 覆盖测试身份的认证头，JSON 值为 null 时删除对应头，也可用 `--remove-header Cookie` 去掉原会话。认证失败或响应不同只代表观察结果，需要结合账号权限、业务预期和实际影响判断。请求或响应 body 中可能含敏感数据，证据目录和原始材料应按凭据文件保护。

显式安装 Java JDK（`java`、`javac`、`jar`）、`jadx`、`apktool`、`file`、`sqlite3`，结合现有 `unzip`、`strings`、`rg`、`jq` 和 `yq` 支持：

- `jadx -d <新目录> <APK/JAR/DEX>`：反编译代码（包含 APK 内多个 DEX），查找接口、认证实现和本地数据处理；混淆或加固包可能需要补充源码。
- `apktool d <APK> -o <新目录>`：解码 Manifest、资源和 smali，检查权限、导出组件、深链、备份及网络配置；声明须结合代码、资源限定符和 Android 版本判断，关键词命中不是漏洞证明。
- `file <文件>`、`unzip -l <压缩包>`：确认类型和内容，再将需要分析的源码或资源提取到独立目录；不要直接运行来历未知的安装包和构建脚本。
- `sqlite3 -readonly <数据库>`：只读检查本地表结构与保存的数据；JSON/YAML/文本配置使用 `jq`、`yq`、`rg`。

以上覆盖客户端服务端接口重放和本地静态分析，不验证 APK 签名真实性或 Android 运行时行为；拆分 APK 需要完整 split 集。抓包由已有测试设备/代理导出 HAR 或原始请求；镜像不新增设备代理、Android/iOS 原生页面操作或自动登录。设备签名、客户端证书绑定、过期令牌等仍可能使离开原设备的请求无法重放。

## Python 与云 CLI

`/opt/pwnmesh-venv/bin` 已加入 PATH，`python`/`python3` 和 `pip`/`pip3` 默认使用该虚拟环境。两个虚拟环境均使用 Python 3.13，避免 pwntools 4.15.0 对 Python 3.14 部分字节码不兼容的问题。预装：

| 包 | 默认版本 | 用法 |
| --- | --- | --- |
| pwntools | 4.15.0 | Python 中 `from pwn import ...`，支持 amd64 汇编 |
| pymongo | 4.18.1 | Python 中 `import pymongo`，提供 MongoDB 客户端和 BSON 编解码 |
| awscli | 1.46.1 | AWS CLI v1，命令 `aws`；预装 groff-base 支持离线帮助 |

`tccli` 3.1.173.1 使用独立的 `/opt/tccli-venv`，并通过 `/usr/local/bin/tccli` 加入 PATH，避免其 SDK 依赖与任务追加的 Python 包互相影响。`REQUESTS_CA_BUNDLE` 指向 `/etc/ssl/certs/ca-certificates.crt`，requests 使用系统 CA。

`aliyun` 3.5.1 来自官方 Linux amd64 发布包，构建时校验固定 SHA256。三个云 CLI 的版本命令均可离线执行；访问云服务时由任务提供相应凭据和网络连接。镜像不预置云凭据。

任务可继续用 `python -m pip install ...` 向共享虚拟环境安装依赖。

## 浏览器

Node.js 和 npm 由 Kali APT 安装，提供 `node`、`npm`、`npx`；自检要求 Node.js 至少为 20。全局安装 `@playwright/cli@latest`，命令为 `playwright-cli`；构建时通过 `playwright-cli install` 下载 Chromium，并安装浏览器运行库及字体。

浏览器文件位于 `/opt/ms-playwright`；`/usr/local/bin/pwnmesh-chromium` 指向该版本的 Chromium。环境变量默认选择 Chromium、无头模式和 `sandbox=false`，适配镜像默认的 root 用户。CLI 通过 `PLAYWRIGHT_MCP_EXECUTABLE_PATH` 使用固定入口，可在任意工作目录运行，无需再次安装浏览器。

`latest` 在安装层实际执行时解析；Docker 缓存命中时沿用已有版本。更新方式和离线验证命令见仓库中的 `container/README.md`。

## 安装范围

预装 Kali headless 工具集、上述补充工具及 PwnMesh 二进制；不额外复制原竞赛环境的知识库、PoC、`AGENTS.md`、`CLAUDE.md` 和技能目录。Playwright 初始化在临时目录完成，不向项目注入 skills 或配置。

镜像通过 `PWNMESH_WORKER_ENVIRONMENT=kali-headless` 标识环境。Worker 在初始任务提示词中注入简短的 Kali/headless 说明和实际工作目录；执行阶段另提示可尝试 nuclei、ffuf 等命令，并以实际输出确认可用性。未设置或不识别该标记时，不声明 Kali 或预装工具集。收尾和修复阶段沿用原始说明，不重复注入。

此文档全文不自动注入模型提示词。
