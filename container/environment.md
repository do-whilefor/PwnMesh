# PwnMesh Worker Environment

This document describes the full `pwnmesh-worker:dev` environment used by the public build configuration, including tools for Web and APK/JAR analysis.
See [Dockerfile](Dockerfile) for the build definition and [check-worker.sh](check-worker.sh) for the offline self-check.

- Based on the official Kali rolling image (`linux/amd64`), with the `kali-linux-headless` metapackage and its required tool dependencies. The Go `pwnmesh worker` runs the agent loop.
- `/workspace` is the shared working directory for a project. `/home/kali/workspace` points to the same directory, which is initialized as a Git repository.
- `/workspace/.pwnmesh/runs/<run-id>` stores the task, session, and tool outputs for each run.
- The default user is root to support private task files written by the Dispatcher. The `kali` user and passwordless sudo remain available for manual use.
- The timezone is `Asia/Shanghai`, and Python output is unbuffered.
- CA certificates are supplied by the control image before the first APT request to support HTTPS mirrors. Kali's `ca-certificates` manages certificates after the build.

## Base Tools

bash, curl, wget, ripgrep (`rg`), fd (`fdfind`), Python 3.13, pip, venv, jq, git, coreutils, procps (`ps`), iproute2 (`ip`), dnsutils (`dig`), zip/unzip, sudo, CA certificates, and timezone data. Also includes binutils, cpp, C/C++ build tools, and Python 3.13 development dependencies to support native assembly with pwntools and Python package installation from source.

Also explicitly installs bsdextrautils (`column`, `hexdump`), Node.js/npm, iputils-ping (`ping`), sshpass, ncat, rlwrap, yq, krb5-user (`kinit`, `klist`), and adb. `yq` is Kali's jq-style Python implementation; use `yq -r '.name' file.yaml` to read a field.

The headless toolset includes commands such as nmap and sqlmap and their packaged data. APT does not install additional Recommends. Services are not started automatically during package installation. PwnMesh remains the container entrypoint, and systemd does not run; tasks can start the required service processes directly.

The file capabilities shipped with Kali's Nmap package are removed to prevent startup failures caused by `CAP_NET_ADMIN` exceeding Docker's default permissions. The default root process uses the container's existing permissions.

## Client APIs and Local Materials

Projects accept uploads such as HAR files, raw HTTP requests, APKs, source archives, configurations, and databases, including binary files. The workspace path returned by an upload is the Worker's input path. Uploading does not extract or execute files. Use task requirements to specify the authorized API scope, analysis goals, and identities of the different test accounts.

`pwn-http inspect <file>` reads a HAR file or raw HTTP request offline and outputs request indices, methods, redacted URLs, header names, and body sizes. `pwn-http replay <file> --index 1 --output <new-directory>` sends the selected complete request once and saves request/response evidence; it does not automatically follow redirects or retry. Raw requests with relative paths require `--base-url <origin>`. HTTP error responses are also saved as evidence. Replay is rejected if a body is missing or cannot be fully reconstructed.

Use `--headers <JSON-file>` to override authentication headers for the test identity. A JSON value of null removes the corresponding header; `--remove-header Cookie` can also remove the original session. Authentication failures or differing responses are observations that must be assessed against account permissions, expected application behavior, and actual impact. Request or response bodies may contain sensitive data; protect evidence directories and source materials as you would credential files.

Java JDK (`java`, `javac`, `jar`), `jadx`, `apktool`, `file`, and `sqlite3` are explicitly installed. Together with the existing `unzip`, `strings`, `rg`, `jq`, and `yq`, they support:

- `jadx -d <new-directory> <APK/JAR/DEX>`: Decompile code, including multiple DEX files within an APK, to find APIs, authentication implementations, and local data handling. Obfuscated or hardened packages may require additional source code.
- `apktool d <APK> -o <new-directory>`: Decode the Manifest, resources, and smali to inspect permissions, exported components, deep links, backups, and network configuration. Assess declarations alongside the code, resource qualifiers, and Android version; keyword matches are not proof of a vulnerability. For SDK versions, also inspect `sdkInfo` in the decoded directory's `apktool.yml`, since `uses-sdk` may have been moved there from the decoded Manifest.
- `file <file>`, `unzip -l <archive>`: Confirm file types and contents, then extract the source code or resources to analyze into a separate directory. Do not directly run installation packages or build scripts of unknown origin.
- `sqlite3 -readonly <database>`: Inspect local table schemas and stored data in read-only mode. Use `jq`, `yq`, and `rg` for JSON, YAML, and text configurations.

These tools cover replaying client-to-server API requests and local static analysis. They do not verify APK signature authenticity or Android runtime behavior; split APKs require the complete split set. Export captured traffic as HAR files or raw requests from existing test devices or proxies. The image does not add device agents, native Android/iOS UI interaction, or automatic login. Device signatures, client certificate binding, expired tokens, and similar constraints may still prevent replaying requests outside the original device.

## Python and Cloud CLIs

`/opt/pwnmesh-venv/bin` is on PATH, so `python`/`python3` and `pip`/`pip3` use this virtual environment by default. Both virtual environments use Python 3.13 to avoid pwntools 4.15.0 incompatibilities with some Python 3.14 bytecode. Preinstalled packages:

| Package | Default version | Usage |
| --- | --- | --- |
| pwntools | 4.15.0 | `from pwn import ...` in Python; supports amd64 assembly |
| pymongo | 4.18.1 | `import pymongo` in Python; provides a MongoDB client and BSON encoding/decoding |
| awscli | 1.46.1 | AWS CLI v1, invoked as `aws`; groff-base is preinstalled for offline help |

`tccli` 3.1.173.1 uses a separate `/opt/tccli-venv` and is available on PATH through `/usr/local/bin/tccli`, isolating its SDK dependencies from Python packages added by tasks. `REQUESTS_CA_BUNDLE` points to `/etc/ssl/certs/ca-certificates.crt` so requests uses the system CA certificates.

`aliyun` 3.5.1 comes from the official Linux amd64 release archive, with a pinned SHA256 verified during the build. Version commands for all three cloud CLIs work offline. Tasks must provide the credentials and network connectivity required to access cloud services. No cloud credentials are included in the image.

Tasks can install additional dependencies into the shared virtual environment with `python -m pip install ...`.

## Browser

Kali APT installs Node.js and npm, providing `node`, `npm`, and `npx`. The self-check requires Node.js 20 or later. `@playwright/cli@latest` is installed globally as `playwright-cli`. The build downloads Chromium through `playwright-cli install` and installs browser runtime libraries and fonts.

Browser files are stored in `/opt/ms-playwright`, and `/usr/local/bin/pwnmesh-chromium` points to that Chromium version. Environment variables default to Chromium, headless mode, and `sandbox=false` to accommodate the image's default root user. The CLI uses the fixed entrypoint through `PLAYWRIGHT_MCP_EXECUTABLE_PATH` and can run from any working directory without reinstalling the browser.

`latest` is resolved when the installation layer executes; a Docker cache hit reuses the existing version. See [Dockerfile](Dockerfile) for installation steps. To resolve the version again, run `docker build --no-cache -f container/Dockerfile -t pwnmesh-worker:dev .` from the repository root after building `pwnmesh:dev`. See [check-worker.sh](check-worker.sh) for offline checks, then run `docker run --rm --pull never --network none --init --entrypoint /usr/local/share/pwnmesh/check-worker.sh pwnmesh-worker:dev` after the build.

## Installation Scope

The image includes the Kali headless toolset, the additional tools listed above, and the PwnMesh binary. It does not copy the original competition environment's knowledge base, PoCs, `AGENTS.md`, `CLAUDE.md`, or skill directories. Playwright initialization runs in a temporary directory and does not inject skills or configuration into projects.

The image identifies its environment with `PWNMESH_WORKER_ENVIRONMENT=kali-headless`. The Worker adds a brief Kali/headless description and the actual working directory to the initial task prompt. During execution, it also suggests trying commands such as nuclei and ffuf and checking their actual output to confirm availability. If the marker is absent or unrecognized, the prompt makes no claims about Kali or preinstalled tools. Concluding and repair phases retain the original description without injecting it again.

The full text of this document is not automatically injected into model prompts.
