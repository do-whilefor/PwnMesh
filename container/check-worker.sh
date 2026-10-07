#!/bin/sh
set -eu

. /etc/os-release
test "$ID" = kali
test "$(id -u)" = 0
test "$(getent passwd kali | cut -d: -f6)" = /home/kali
test "$(readlink -f /home/kali/workspace)" = /workspace
test "$TZ" = Asia/Shanghai
test "$PYTHONUNBUFFERED" = 1
test -s /etc/ssl/certs/ca-certificates.crt
for package in ca-certificates kali-linux-headless bsdextrautils iputils-ping sshpass ncat rlwrap yq krb5-user adb nodejs npm jq ripgrep fd-find default-jdk-headless jadx apktool file sqlite3; do
    dpkg-query -W -f '${Status}\n' "$package" | grep -Fx 'install ok installed' >/dev/null
done
for tool in bash curl wget rg fd python python3 pip pip3 jq git cat ps ip dig unzip zip sudo as objcopy cpp aws tccli aliyun node npm playwright-cli \
    column hexdump ping sshpass ncat rlwrap yq kinit klist adb nmap sqlmap java javac jar jadx apktool file strings sqlite3 pwn-http; do
    command -v "$tool" >/dev/null
done
su -s /bin/sh kali -c 'test -w /workspace && test "$(sudo -n id -u)" = 0'
# /workspace 是 git 仓库且 Worker 以 root 运行，而目录属主是 kali。
# 必须能真正执行 git 操作，否则依赖 git 的任务会以 128 (dubious ownership) 失败。
# 仅检查 git 命令存在无法覆盖这一点，故这里实际执行一次仓库操作。
git -C /workspace status --short >/dev/null
smoke_dir=$(mktemp -d /workspace/.pwnmesh-smoke.XXXXXX)
trap 'rm -rf -- "$smoke_dir"' EXIT HUP INT TERM
file="$smoke_dir/probe.txt"
printf 'pwnmesh-worker-smoke\n' > "$file"
rg -q '^pwnmesh-worker-smoke$' "$file"
fd --hidden --no-ignore --type f '^probe\.txt$' "$smoke_dir" | grep -Fx "$file" >/dev/null
test "$(cat "/home/kali/workspace/${smoke_dir##*/}/probe.txt")" = pwnmesh-worker-smoke
bash -c 'test "$BASH_VERSION"'
ip -j link show lo | jq -e 'any(.[]; .ifname == "lo")' >/dev/null
ping -n -c 1 -W 2 127.0.0.1 >/dev/null
dig -v >/dev/null
pip --version
pip3 --version
python3 -m pip --version
python3 -m pip check
/opt/tccli-venv/bin/python -m pip check
test "$(readlink -f "$(command -v tccli)")" = /opt/tccli-venv/bin/tccli
node -e 'if (Number(process.versions.node.split(".")[0]) < 20) process.exit(1)'
test "$PLAYWRIGHT_MCP_BROWSER" = chromium
test "$PLAYWRIGHT_MCP_HEADLESS" = true
test "$PLAYWRIGHT_MCP_SANDBOX" = false
test "$PLAYWRIGHT_MCP_EXECUTABLE_PATH" = /usr/local/bin/pwnmesh-chromium
test -x "$PLAYWRIGHT_MCP_EXECUTABLE_PATH"
test "$PLAYWRIGHT_BROWSERS_PATH" = /opt/ms-playwright
test -d "$PLAYWRIGHT_BROWSERS_PATH"

# 仅连接回环地址，因此构建及 --network none 下的测试均无需外网。
python - "$smoke_dir" <<'PY'
import http.server
import hashlib
import importlib.metadata
import json
import os
import pathlib
import re
import ssl
import subprocess
import sys
import threading
import venv
import xml.etree.ElementTree as ET
import zipfile

assert sys.prefix == "/opt/pwnmesh-venv", sys.prefix
assert sys.version_info[:2] == (3, 13), sys.version
assert ssl.create_default_context().cert_store_stats()["x509_ca"] > 0
smoke_dir = pathlib.Path(sys.argv[1])
os.environ["XDG_CACHE_HOME"] = str(smoke_dir / ".cache")
os.environ["PWNLIB_NOTERM"] = "1"

# Exercise the added text tools and YAML bridge with local fixture bytes.
columns = subprocess.check_output(
    ["column", "-t", "-s", ","], input="name,value\npwnmesh,42\n", text=True, timeout=10,
).splitlines()
assert [line.split() for line in columns] == [["name", "value"], ["pwnmesh", "42"]], columns
assert columns[0].index("value") == columns[1].index("42"), columns
assert subprocess.check_output(
    ["hexdump", "-v", "-e", '1/1 "%02x"'], input=b"\x00AB\xff", timeout=10,
) == b"004142ff"
assert subprocess.check_output(
    ["yq", "-r", ".service.name"], input="service:\n  name: pwnmesh-worker\n", text=True, timeout=10,
).strip() == "pwnmesh-worker"

# Build synthetic local inputs; decompile actual bytecode and decode a real APK.
# Nothing is downloaded and no device or production package is needed.
java_source = smoke_dir / "ClientProbe.java"
java_source.write_text('package example; public class ClientProbe { public String endpoint() { return "https://api.example.invalid/v1"; } }\n')
subprocess.run(["javac", "--release", "8", "-Xlint:-options", "-d", str(smoke_dir), str(java_source)], check=True, timeout=30)
java_archive = smoke_dir / "client.jar"
subprocess.run(["jar", "cf", str(java_archive), "-C", str(smoke_dir), "example/ClientProbe.class"], check=True, timeout=30)
java_decoded = smoke_dir / "java-decoded"
subprocess.run(["jadx", "--no-res", "-d", str(java_decoded), str(java_archive)], check=True, timeout=90)
assert "https://api.example.invalid/v1" in (java_decoded / "sources/example/ClientProbe.java").read_text()

apk_source = smoke_dir / "apk-source"
for directory in ("smali/example", "smali_classes2/example", "res/xml", "assets"):
    (apk_source / directory).mkdir(parents=True)
(apk_source / "apktool.yml").write_text("""version: 2.7.0
apkFileName: client.apk
isFrameworkApk: false
usesFramework:
  ids: [1]
sdkInfo:
  minSdkVersion: '21'
  targetSdkVersion: '28'
versionInfo:
  versionCode: '1'
  versionName: '1.0'
""")
(apk_source / "AndroidManifest.xml").write_text('''<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android" package="example.clientprobe" android:versionCode="1" android:versionName="1.0">
  <uses-sdk android:minSdkVersion="21" android:targetSdkVersion="28"/>
  <uses-permission android:name="android.permission.INTERNET"/>
  <application android:label="Client probe" android:allowBackup="false" android:debuggable="true" android:networkSecurityConfig="@xml/network_security_config">
    <activity android:name="example.ProbeActivity" android:exported="true">
      <intent-filter>
        <action android:name="android.intent.action.VIEW"/>
        <category android:name="android.intent.category.BROWSABLE"/>
        <data android:scheme="probe" android:host="client"/>
      </intent-filter>
    </activity>
    <provider android:name="example.PrivateProvider" android:authorities="example.clientprobe.data" android:exported="false"/>
  </application>
</manifest>
''')
(apk_source / "res/xml/network_security_config.xml").write_text('''<?xml version="1.0" encoding="utf-8"?>
<network-security-config>
  <base-config cleartextTrafficPermitted="false"/>
  <domain-config cleartextTrafficPermitted="true">
    <domain includeSubdomains="true">api.example.invalid</domain>
  </domain-config>
</network-security-config>
''')
(apk_source / "assets/client.json").write_text('{"api":"https://api.example.invalid/asset"}\n')
(apk_source / "smali/example/ClientProbe.smali").write_text('''.class public Lexample/ClientProbe;
.super Ljava/lang/Object;
.method public static endpoint()Ljava/lang/String;
    .registers 1
    const-string v0, "https://api.example.invalid/mobile"
    return-object v0
.end method
''')
(apk_source / "smali_classes2/example/SecondaryProbe.smali").write_text('''.class public Lexample/SecondaryProbe;
.super Ljava/lang/Object;
.method public static endpoint()Ljava/lang/String;
    .registers 1
    const-string v0, "https://api.example.invalid/secondary"
    return-object v0
.end method
''')
apk_file = smoke_dir / "client.apk"
apk_framework = smoke_dir / "apk-framework"
subprocess.run(["apktool", "b", str(apk_source), "-p", str(apk_framework), "-o", str(apk_file)], check=True, timeout=90)
apk_digest = hashlib.sha256(apk_file.read_bytes()).digest()
with zipfile.ZipFile(apk_file) as archive:
    assert {"classes.dex", "classes2.dex", "resources.arsc", "AndroidManifest.xml", "assets/client.json"} <= set(archive.namelist())
apk_decoded = smoke_dir / "apk-decoded"
subprocess.run(["apktool", "d", str(apk_file), "-p", str(apk_framework), "-o", str(apk_decoded)], check=True, timeout=90)
android = "{http://schemas.android.com/apk/res/android}"
manifest = ET.parse(apk_decoded / "AndroidManifest.xml").getroot()
assert manifest.attrib["package"] == "example.clientprobe"
assert manifest.find("uses-permission").attrib[android + "name"] == "android.permission.INTERNET"
application = manifest.find("application")
assert application.attrib[android + "allowBackup"] == "false"
assert application.attrib[android + "debuggable"] == "true"
assert application.attrib[android + "networkSecurityConfig"] == "@xml/network_security_config"
activity = application.find("activity")
assert activity.attrib[android + "exported"] == "true"
assert activity.find("intent-filter/data").attrib[android + "scheme"] == "probe"
assert application.find("provider").attrib[android + "exported"] == "false"
network = ET.parse(apk_decoded / "res/xml/network_security_config.xml").getroot()
assert network.find("base-config").attrib["cleartextTrafficPermitted"] == "false"
assert network.find("domain-config").attrib["cleartextTrafficPermitted"] == "true"
assert network.find("domain-config/domain").text.strip() == "api.example.invalid"
assert json.loads((apk_decoded / "assets/client.json").read_text())["api"].endswith("/asset")
assert "api.example.invalid/mobile" in (apk_decoded / "smali/example/ClientProbe.smali").read_text()
assert "api.example.invalid/secondary" in (apk_decoded / "smali_classes2/example/SecondaryProbe.smali").read_text()
apk_java = smoke_dir / "apk-java"
subprocess.run(["jadx", "--no-res", "-d", str(apk_java), str(apk_file)], check=True, timeout=90)
assert "api.example.invalid/mobile" in (apk_java / "sources/example/ClientProbe.java").read_text()
assert "api.example.invalid/secondary" in (apk_java / "sources/example/SecondaryProbe.java").read_text()
assert hashlib.sha256(apk_file.read_bytes()).digest() == apk_digest, "analysis changed the original APK"
file_type = subprocess.check_output(["file", "-b", str(apk_file)], text=True, timeout=10).lower()
assert any(kind in file_type for kind in ("archive", "android package")), file_type

# Read a local database without modifying the supplied input.
database = smoke_dir / "client.db"
subprocess.run(["sqlite3", str(database), "CREATE TABLE config(name TEXT, value TEXT); INSERT INTO config VALUES ('api', 'https://api.example.invalid');"], check=True, timeout=10)
assert subprocess.check_output(["sqlite3", "-readonly", str(database), "SELECT value FROM config WHERE name='api';"], text=True, timeout=10).strip() == "https://api.example.invalid"

# Local version paths only: no scans, SSH login, Kerberos tickets or ADB daemon.
for command, label in (
    (["nmap", "--version"], "nmap"),
    (["sshpass", "-V"], "sshpass"),
    (["ncat", "--version"], "ncat"),
    (["rlwrap", "--version"], "rlwrap"),
    (["klist", "-V"], "kerberos"),
    (["adb", "version"], "android debug bridge"),
):
    output = subprocess.check_output(command, stderr=subprocess.STDOUT, text=True, timeout=15)
    assert label in output.lower(), (command, output)

versions = {package: importlib.metadata.version(package) for package in ("pwntools", "pymongo", "awscli")}
for package, version in versions.items():
    assert version, package
    print(f"{package} {version}")
tccli_version = subprocess.check_output(
    ["/opt/tccli-venv/bin/python", "-c", "import importlib.metadata; print(importlib.metadata.version('tccli'))"],
    text=True, timeout=10,
).strip()
assert tccli_version, "tccli has no installed version"

# Resolve Requests' effective CA bundle without preparing or sending a request.
requests_ca_check = """import requests
with requests.Session() as session:
    settings = session.merge_environment_settings('https://example.invalid', {}, None, None, None)
assert settings['verify'] == '/etc/ssl/certs/ca-certificates.crt', settings['verify']
"""
for interpreter in (sys.executable, "/opt/tccli-venv/bin/python"):
    subprocess.run([interpreter, "-c", requests_ca_check], check=True, timeout=10)

# Exercise native assembly and encoding without a target process or service.
from pwn import asm, context, cyclic, cyclic_find
from pwnlib.util.safeeval import const
assert const("1") == 1
assert const("[1, 2, 3]") == [1, 2, 3]
with context.local(arch="amd64", os="linux", log_level="error"):
    assert asm("xor eax, eax; ret") == b"\x31\xc0\xc3"
    pattern = cyclic(64)
    assert cyclic_find(pattern[24:28]) == 24

import pymongo
from bson import BSON, ObjectId
document = {"_id": ObjectId("0123456789abcdef01234567"), "count": 3, "tags": ["pwnmesh", "离线"]}
assert BSON(BSON.encode(document)).decode() == document
assert pymongo.version == versions["pymongo"], pymongo.version

# Version commands do not require cloud credentials or make service requests.
for command, version in (
    (["aws", "--version"], "aws-cli/" + versions["awscli"]),
    (["tccli", "--version"], tccli_version),
):
    output = subprocess.check_output(command, stderr=subprocess.STDOUT, text=True, timeout=15)
    assert version in output, (command, output)
    print(output.strip())
aws_help = subprocess.check_output(
    ["aws", "help"], stderr=subprocess.STDOUT, text=True, timeout=30,
    env=dict(os.environ, MANPAGER="cat", PAGER="cat", AWS_EC2_METADATA_DISABLED="true"),
)
# groff's terminal output may encode bold/underlining with backspace overstrikes.
aws_help = re.sub(r".\x08", "", aws_help)
assert "SYNOPSIS" in aws_help, "AWS CLI help did not render its synopsis"
aliyun_version = subprocess.check_output(["aliyun", "version"], text=True, timeout=15).strip()
assert re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?", aliyun_version), aliyun_version
print("aliyun " + aliyun_version)
subprocess.run(["playwright-cli", "--version"], check=True, timeout=15)

venv_path = smoke_dir / "venv"
venv.create(venv_path, with_pip=True)
subprocess.run([str(venv_path / "bin/python"), "-m", "pip", "--version"], check=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8" if self.path == "/browser" else "text/plain")
        self.end_headers()
        if self.path == "/browser":
            self.wfile.write(b'<!doctype html><title>PwnMesh browser smoke</title><p id="result">pending</p>'
                            b'<script>document.querySelector("#result").textContent = "chromium-script-ran";</script>')
        else:
            self.wfile.write(b"pwnmesh-worker-smoke\n")

    def log_message(self, *args):
        pass


with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_port}/"
    try:
        for command in (
            ["curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "5", url],
            ["wget", "--no-proxy", "--quiet", "--timeout=5", "--tries=1", "-O", "-", url],
        ):
            assert subprocess.check_output(command, timeout=10) == b"pwnmesh-worker-smoke\n"
        capture = smoke_dir / "client.har"
        capture.write_text(json.dumps({"log": {"version": "1.2", "entries": [{"request": {
            "method": "GET", "url": url, "headers": [], "bodySize": 0,
        }}]}}))
        inspected = json.loads(subprocess.check_output(["pwn-http", "inspect", str(capture)], text=True, timeout=10))
        assert len(inspected) == 1 and inspected[0]["replayable"], inspected
        evidence = smoke_dir / "http-evidence"
        replayed = json.loads(subprocess.check_output(["pwn-http", "replay", str(capture), "--index", "1", "--output", str(evidence)], text=True, timeout=10))
        assert replayed["status"] == 200, replayed
        assert (evidence / "response.body").read_bytes() == b"pwnmesh-worker-smoke\n"
        assert json.loads((evidence / "response.json").read_text())["status"] == 200
        session = "pwnmesh-smoke-" + str(os.getpid())
        cli = ["playwright-cli", "-s=" + session]
        # Use the installed CLI and its Chromium defaults, not a separate Node API.
        # Its run-code command exits nonzero when either browser assertion fails.
        try:
            subprocess.run(cli + ["open", url + "browser"], cwd=smoke_dir, check=True, timeout=45)
            subprocess.run(cli + ["run-code", """async page => {
                if ((await page.title()) !== 'PwnMesh browser smoke') throw new Error('browser title mismatch');
                if ((await page.locator('#result').innerText()) !== 'chromium-script-ran') throw new Error('page script did not execute');
            }"""], cwd=smoke_dir, check=True, timeout=20)
        finally:
            subprocess.run(cli + ["close"], cwd=smoke_dir, check=True, timeout=20)
    finally:
        server.shutdown()
        thread.join()
PY

# 不恢复原竞赛环境的额外知识库或项目 Agent 指令。
for path in /home/kali/knowledges /home/kali/tools /home/kali/pocs /workspace/.agents /workspace/.claude /workspace/AGENTS.md /workspace/CLAUDE.md; do
    test ! -e "$path"
done
/usr/local/bin/pwnmesh worker --help 2>&1 | grep -F -- '-job' >/dev/null
test -r /usr/local/share/pwnmesh/environment.md
printf 'Kali Worker smoke passed: OS=%s user=%s workspace=%s\n' "$ID" "$(id -un)" "$PWD"
