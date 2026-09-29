<div align="center">

# X-Loom

**面向 CTF、授权渗透测试与代码审计的多 Worker AI 协作探索系统**

*Plan centrally. Explore in parallel. Keep the evidence.*

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Worker-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![Platform](https://img.shields.io/badge/Platform-Linux-555?logo=linux&logoColor=white)](#环境要求)
[![License](https://img.shields.io/badge/License-PolyForm_Noncommercial_1.0.0-orange)](./LICENSE)

[项目简介](#项目简介) · [核心能力](#核心能力) · [快速开始](#快速开始) · [使用方式](#使用方式) · [使用限制](#使用限制与安全提示) · [授权说明](#授权说明)

</div>

---

## 项目简介

**X-Loom** 是一个以 Go 构建的多 Worker AI 协作探索系统，面向 **CTF 题目研究、明确授权的渗透测试和代码安全审计** 等需要持续分析、多路径探索和工具辅助验证的任务。

系统由 **Server、Dispatcher、Worker 和 Web 工作台**构成：Server 接收任务并维护共享状态；Dispatcher 按任务阶段与容量配置调度执行；Worker 在 Docker 容器中运行 Agent 与工具，执行具体探索。与将所有工作放在单个对话中的方式不同，X-Loom 将任务规划、执行与状态记录分开，便于管理多个探索方向和查看执行过程。

X-Loom 是用于辅助研究的执行框架，**不是一键自动确认漏洞的扫描器**。模型生成的判断、工具输出与安全结论均需结合授权范围和原始证据人工复核。

## 核心能力

- **多 Worker 协作：** Dispatcher 按配置管理任务并发与 Worker 容量，按需创建执行容器。
- **分阶段探索：** 提供 `bootstrap`、`reason`、`explore` 等任务类型及相应的执行参数。
- **共享任务状态：** Server 统一承载项目及执行状态，减少完全依赖单次模型上下文的问题。
- **容器化执行：** Worker 镜像基于 Kali Linux，集成安全研究工具、运行依赖及 Agent 工作目录。
- **Web 工作台：** 从浏览器访问服务、提交任务并查看运行情况。
- **可配置模型接入：** 通过环境变量配置模型凭证、接口地址与模型名称。

### 运行结构

```text
                   用户 / Web 工作台
                          │
                          ▼
                 ┌─────────────────┐
                 │ Server          │
                 │ 任务入口 / 状态  │
                 └────────┬────────┘
                          │
                          ▼
                 ┌─────────────────┐
                 │ Dispatcher      │
                 │ 调度 / 并发控制 │
                 └────────┬────────┘
                          │ 按需创建 Docker 容器
            ┌─────────────┼─────────────┐
            ▼             ▼             ▼
       ┌─────────┐   ┌─────────┐   ┌─────────┐
       │ Worker A│   │ Worker B│   │ Worker C│
       │ Agent   │   │ Agent   │   │ Agent   │
       │ + Tools │   │ + Tools │   │ + Tools │
       └────┬────┘   └────┬────┘   └────┬────┘
            └─────────────┼─────────────┘
                          │ 执行结果 / 状态回传
                          ▼
                        Server
                          │
                          ▼
                       Web 工作台
```

> 结构图为主要组件关系示意，不代表所有内部调用都必须经过图示的单一路径。

## 适用场景

| 场景 | 典型用途 |
| --- | --- |
| CTF / 靶场 | 题目理解、线索拆解、多方向探索及解题过程整理 |
| 授权渗透测试 | 目标信息分析、验证任务拆分、工具辅助执行及证据整理 |
| 代码审计 | 代码结构理解、危险调用与数据流追踪、候选问题交叉验证 |
| 安全研究 | 长任务探索、不同假设并行验证、执行状态追踪 |

**适用场景不等于自动具备授权。** 任务目标、测试手段、时间窗口与影响范围仍须由使用者事先确认。

## 快速开始

### 环境要求

- **Linux 主机**，可正常运行 Docker Engine 和 Docker Compose 插件。
- 可访问所配置的模型 API，并具备有效的访问凭证。
- 可拉取基础镜像与 Worker 构建所需依赖；首次构建 Kali 工具镜像可能占用较多时间、磁盘和网络流量。
- 当前 Worker 镜像显式使用 `linux/amd64`；其他 CPU 架构上的构建和执行兼容性请自行验证。
- 仅在可信主机上部署：Dispatcher 需要挂载宿主机的 Docker Socket。

### 1. 获取代码

```bash
git clone https://github.com/do-whilefor/X-Loom.git
cd X-Loom
```

### 2. 准备配置

```bash
cp .env.example .env
cp dispatch.example.yaml dispatch.yaml
```

编辑 `.env`，填写所使用的模型服务信息：

```dotenv
ANTHROPIC_AUTH_TOKEN=your_token
ANTHROPIC_BASE_URL=your_api_base_url
ANTHROPIC_DEFAULT_FABLE_MODEL=your_model_name
```

`ANTHROPIC_AUTH_TOKEN` 是模型访问凭证；其余两项分别指定 API 地址与模型名称。请使用与你的服务商及接口协议兼容的配置，**不要将真实 Token 提交至 Git 仓库**。

按需要修改 `dispatch.yaml` 中的并发、超时和 Worker 配置。默认 Worker 镜像应保持：

```yaml
container:
  image: xloom-worker:dev
```

### 3. 构建镜像

**先构建主镜像，再构建 Worker 镜像**；后者依赖本地的 `xloom:dev`：

```bash
docker build -t xloom:dev .
docker build -f container/Dockerfile -t xloom-worker:dev .
```

### 4. 启动服务

```bash
docker compose up -d --no-build
docker compose ps
```

默认访问地址：**http://127.0.0.1:8000**。如果需要调整宿主机端口，可在 `.env` 中设置 `XLOOM_PORT`，例如 `XLOOM_PORT=8080`。

Compose 启动的是 **Server 和 Dispatcher**；具体 Worker 容器由 Dispatcher 在收到任务后动态创建，并非固定常驻的 Compose 服务。

### 5. 查看日志与停止

```bash
# 查看运行日志
docker compose logs -f server dispatcher

# 停止并移除 Compose 服务容器
docker compose down
```

默认使用命名卷 `xloom-data` 保存 Server 数据。`docker compose down` 通常保留该卷；执行带 `-v` 的删除命令前，请确认是否仍需要其中的任务数据。

## 使用方式

1. 打开 Web 工作台，确认 Server 与 Dispatcher 正常运行。
2. 在合法授权范围内创建项目或提交任务，说明研究目标、范围、限制条件和预期结果。
3. Dispatcher 根据任务类型和并发配置创建 Worker；Worker 使用镜像中的 Agent 与工具开展分析或验证。
4. 在工作台查看任务状态与执行输出，结合请求响应、代码位置和其他原始证据复核结果。
5. 对未证实的问题保持待验证状态；对可能造成破坏或超出授权范围的动作停止执行并重新确认授权。

Worker 镜像内的工具与知识资料以 [`container/Dockerfile`](./container/Dockerfile) 为准。工具已安装不表示任意目标都可测试，也不保证每项工具、模板和 PoC 在所有环境下都可直接使用。

## 使用限制与安全提示

- **仅限合法授权：** 只允许针对自己拥有、明确获得授权的系统，以及赛事明确允许的 CTF / 靶场环境使用。不得超出授权的资产、账号、接口、时间或测试方式。
- **禁止滥用：** 不得用于未经授权的入侵、凭证攻击、数据窃取、破坏、持久化、横向移动、拒绝服务，或其他违法和侵害第三方权益的行为。
- **模型输出需复核：** AI 生成的命令、漏洞判断、攻击路径与报告可能错误；扫描命中或异常响应本身不代表漏洞已经确认。
- **容器并非完整安全边界：** 当前 Dispatcher 挂载 `/var/run/docker.sock`，可操作宿主机 Docker；Worker 镜像当前以 root 运行。请勿直接部署到不可信、多租户或承载敏感业务的生产主机。
- **注意信息保护：** 输入模型的目标资料、源码、日志、Cookie、凭证及其他数据，须符合授权、保密要求和模型服务的数据处理约定。
- **资源与环境：** 多 Worker 并行执行会消耗模型额度、CPU、内存、磁盘及网络资源；请根据主机和目标环境调整并发上限。

项目作者不对使用者是否取得测试授权作出保证；使用者须自行对具体目标、操作和产生的后果负责。

## 授权说明

X-Loom 采用 **[PolyForm Noncommercial License 1.0.0](./LICENSE)**，是一份带有非商业用途限制的源码可用许可证，**不是 OSI 批准的开源许可证**。

在完整许可证允许的范围内，可进行非商业学习、研究、实验、修改和分发。未经权利人另行授权，不得将本项目用于商业产品、商业服务、SaaS、收费安全服务或其他商业用途。是否属于许可证所允许的用途，应以许可证全文及具体使用情形为准。

商业使用、其他特别授权或许可问题，请通过仓库 Issues 与项目维护者联系；Issues 沟通本身不构成授权。完整法律条款以仓库 [`LICENSE`](./LICENSE) 为准。

---

<div align="center">

**X-Loom · For authorized security research only.**

</div>
