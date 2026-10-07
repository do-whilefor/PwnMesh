#!/usr/bin/env python3
"""Inspect client captures locally; replay one explicitly selected HTTP request."""

import argparse
import base64
import binascii
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import sys
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit


TOKEN = re.compile(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
SENSITIVE = re.compile(r"authorization|cookie|token|secret|password|api[-_]?key", re.I)


class CaptureError(ValueError):
    pass


def read_json(path):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8-sig"))
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise CaptureError("invalid UTF-8 JSON file") from exc


def checked_url(url):
    if not isinstance(url, str) or any(ord(c) < 33 or ord(c) > 126 for c in url):
        raise CaptureError("URL must be an ASCII URL with escaped spaces/non-ASCII characters")
    try:
        parsed = urlsplit(url)
        if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username is not None:
            raise ValueError()
        _ = parsed.port
    except ValueError as exc:
        raise CaptureError("URL requires an http(s) origin without embedded credentials") from exc
    if "#" in url:
        raise CaptureError("request URL cannot contain a fragment")
    return parsed


def safe_url(url):
    parsed = checked_url(url)
    query = urlencode([(key, "[redacted]") for key, _ in parse_qsl(parsed.query, keep_blank_values=True)])
    return urlunsplit((parsed.scheme, parsed.netloc, parsed.path, query, ""))


def checked_headers(headers):
    result = []
    if not isinstance(headers, list):
        raise CaptureError("headers must be a list")
    for item in headers:
        if not isinstance(item, dict):
            raise CaptureError("header requires name and value")
        name, value = item.get("name"), item.get("value")
        if not isinstance(name, str) or not TOKEN.fullmatch(name):
            raise CaptureError("invalid header name (HTTP/2 pseudoheaders are unsupported)")
        if not isinstance(value, str) or any(ord(c) < 32 and c != "\t" or ord(c) == 127 for c in value):
            raise CaptureError("invalid header value")
        try:
            value.encode("latin-1")
        except UnicodeError as exc:
            raise CaptureError("header value cannot be represented as HTTP/1 bytes") from exc
        result.append((name, value))
    return result


def values(headers, name):
    return [value for key, value in headers if key.lower() == name.lower()]


def validate(request):
    method, url, headers, body = request["method"], request["url"], request["headers"], request["body"]
    if not isinstance(method, str) or not TOKEN.fullmatch(method) or method.upper() in ("CONNECT", "PRI"):
        raise CaptureError("unsupported or invalid HTTP method")
    parsed = checked_url(url)
    if values(headers, "Transfer-Encoding"):
        raise CaptureError("Transfer-Encoding is unsupported; export a decoded request with an exact body")
    if values(headers, "Upgrade"):
        raise CaptureError("protocol upgrade requests cannot be replayed")
    lengths = values(headers, "Content-Length")
    if len(lengths) > 1 or lengths and (not re.fullmatch(r"[0-9]+", lengths[0]) or (lengths[0].lstrip("0") or "0") != str(len(body))):
        raise CaptureError("Content-Length does not match captured body bytes")
    hosts = values(headers, "Host")
    if len(hosts) > 1:
        raise CaptureError("multiple Host headers are unsupported")
    if hosts:
        try:
            host = checked_url(parsed.scheme + "://" + hosts[0])
        except CaptureError as exc:
            raise CaptureError("invalid Host header") from exc
        default = 443 if parsed.scheme == "https" else 80
        if host.path or host.query or host.hostname.lower() != parsed.hostname.lower() or (host.port or default) != (parsed.port or default):
            raise CaptureError("Host header differs from request URL/base URL")
    return request


def har_request(entry):
    if not isinstance(entry, dict) or not isinstance(entry.get("request"), dict):
        raise CaptureError("HAR entry requires a request object")
    req = entry["request"]
    headers = checked_headers(req.get("headers", []))
    body = b""
    post = req.get("postData")
    if post is not None:
        if not isinstance(post, dict) or not isinstance(post.get("text"), str):
            raise CaptureError("HAR postData requires exact text or base64 text; params alone cannot reconstruct a body")
        encoding = post.get("encoding", post.get("_encoding"))
        if encoding == "base64":
            try:
                body = base64.b64decode(post["text"], validate=True)
            except (ValueError, binascii.Error) as exc:
                raise CaptureError("invalid base64 request body") from exc
        elif encoding is not None:
            raise CaptureError("unsupported HAR body encoding")
        else:
            # HAR text is Unicode, not raw bytes. Only ASCII is unambiguous without
            # an explicit source charset; binary exports must use base64.
            content_types = values(headers, "Content-Type")
            mime = content_types[0] if content_types else post.get("mimeType", "")
            if not isinstance(mime, str):
                raise CaptureError("invalid HAR mimeType")
            if any(value.lower() != "identity" for value in values(headers, "Content-Encoding")):
                raise CaptureError("encoded HAR request bodies require base64 encoded exact bytes")
            charset = re.search(r"charset\s*=\s*[\"']?([\w.-]+)", mime, re.I)
            try:
                body = post["text"].encode(charset.group(1) if charset else "ascii", errors="strict")
            except (LookupError, UnicodeError) as exc:
                raise CaptureError("ambiguous HAR text encoding; export base64 bytes or provide the source charset") from exc
            if "multipart/" in mime.lower() or "application/octet-stream" in mime.lower():
                raise CaptureError("binary/multipart HAR bodies require base64 encoded exact bytes")
        if not values(headers, "Content-Type") and post.get("mimeType"):
            headers.extend(checked_headers([{"name": "Content-Type", "value": post["mimeType"]}]))
    body_size = req.get("bodySize", -1)
    if not isinstance(body_size, int) or body_size < -1:
        raise CaptureError("invalid HAR bodySize")
    if body_size >= 0 and body_size != len(body):
        raise CaptureError("HAR bodySize does not match captured body bytes")
    if post is None and body_size == -1 and str(req.get("method", "")).upper() in ("POST", "PUT", "PATCH") and not values(headers, "Content-Length"):
        raise CaptureError("HAR omits request body information; export postData or a confirmed zero bodySize")
    if not values(headers, "Cookie") and req.get("cookies"):
        cookies = req["cookies"]
        if not isinstance(cookies, list):
            raise CaptureError("invalid HAR cookies")
        parts = []
        for cookie in cookies:
            if not isinstance(cookie, dict) or not isinstance(cookie.get("name"), str) or not TOKEN.fullmatch(cookie["name"]):
                raise CaptureError("invalid HAR cookie name")
            value = cookie.get("value")
            if not isinstance(value, str) or any(ord(c) < 33 or ord(c) > 126 or c in ';,"\\' for c in value):
                raise CaptureError("ambiguous HAR cookie value; export the original Cookie header")
            parts.append(cookie["name"] + "=" + value)
        headers.append(("Cookie", "; ".join(parts)))
    return validate({"method": req.get("method"), "url": req.get("url"), "headers": headers, "body": body})


def raw_request(data, base_url):
    separator = min((sep for sep in (b"\r\n\r\n", b"\n\n") if sep in data), key=data.index, default=None)
    if separator is None:
        raise CaptureError("raw request requires a blank line before its body")
    head, body = data.split(separator, 1)
    lines = head.decode("latin-1").split("\r\n" if separator == b"\r\n\r\n" else "\n")
    if not lines or len(lines[0].split(" ")) != 3:
        raise CaptureError("invalid HTTP request line")
    method, target, version = lines[0].split(" ")
    if version not in ("HTTP/1.0", "HTTP/1.1"):
        raise CaptureError("raw request must use HTTP/1.0 or HTTP/1.1")
    headers = []
    for line in lines[1:]:
        if line.startswith((" ", "\t")) or ":" not in line:
            raise CaptureError("invalid or folded raw HTTP header")
        name, value = line.split(":", 1)
        headers.append({"name": name, "value": value.strip(" \t")})
    if target.startswith("/"):
        if not base_url:
            raise CaptureError("origin-form raw request requires --base-url with an explicit http(s) origin")
        origin = checked_url(base_url)
        if origin.path not in ("", "/") or origin.query:
            raise CaptureError("--base-url must be an origin without a path or query")
        target = urlunsplit((origin.scheme, origin.netloc, "", "", "")) + target
    return validate({"method": method, "url": target, "headers": checked_headers(headers), "body": body})


def load_capture(path, base_url=None):
    data = Path(path).read_bytes()
    if data.lstrip(b"\xef\xbb\xbf \r\n\t").startswith((b"{", b"[")) or Path(path).suffix.lower() == ".har":
        capture = read_json(path)
        if not isinstance(capture, dict) or not isinstance(capture.get("log"), dict) or not isinstance(capture["log"].get("entries"), list):
            raise CaptureError("HAR requires log.entries array")
        return capture["log"]["entries"], har_request
    return [data], lambda entry: raw_request(entry, base_url)


def describe(request, index):
    return {"index": index, "method": request["method"], "url": safe_url(request["url"]),
            "header_names": [name for name, _ in request["headers"]], "body_bytes": len(request["body"]), "replayable": True}


def override_headers(request, path, removals):
    overrides = read_json(path) if path else {}
    if not isinstance(overrides, dict):
        raise CaptureError("header override file must be a JSON object of header names to strings or null")
    names = list(overrides) + list(removals)
    if any(not isinstance(name, str) or not TOKEN.fullmatch(name) for name in names):
        raise CaptureError("invalid override/removal header name")
    lowered = [name.lower() for name in overrides]
    if len(set(lowered)) != len(lowered):
        raise CaptureError("duplicate case-insensitive override header names")
    removed = {name.lower() for name in removals}
    new_headers = checked_headers([{"name": name, "value": value} for name, value in overrides.items() if value is not None and name.lower() not in removed])
    drop = {name.lower() for name in names}
    request = dict(request, headers=[(key, value) for key, value in request["headers"] if key.lower() not in drop] + new_headers)
    return validate(request)


def metadata_headers(headers):
    def redact(name, value):
        if SENSITIVE.search(name):
            return "[redacted]"
        if name.lower() in ("location", "referer", "content-location"):
            # Relative redirects can also carry tokens; no URL values in metadata.
            return "[redacted]"
        return value
    return [{"name": name, "value": redact(name, value)} for name, value in headers]


def save_private(directory, name, data):
    with os.fdopen(os.open(directory / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as stream:
        stream.write(data)


def save_json(directory, name, data):
    save_private(directory, name, (json.dumps(data, ensure_ascii=True, indent=2) + "\n").encode())


def replay(request, output, timeout):
    if not math.isfinite(timeout) or timeout <= 0 or timeout > 300:
        raise CaptureError("timeout must be greater than zero and at most 300 seconds")
    directory = Path(output)
    # Exclusively create evidence before any network access; never overwrite a run.
    directory.mkdir(mode=0o700)
    os.chmod(directory, 0o700)
    parsed = checked_url(request["url"])
    headers = list(request["headers"])
    if not values(headers, "Host"):
        headers.append(("Host", parsed.netloc))
    if not values(headers, "Content-Length") and (request["body"] or request["method"].upper() in ("POST", "PUT", "PATCH")):
        headers.append(("Content-Length", str(len(request["body"]))))
    save_json(directory, "request.json", {"method": request["method"], "url": safe_url(request["url"]),
              "headers": metadata_headers(headers), "body_bytes": len(request["body"]),
              "body_sha256": hashlib.sha256(request["body"]).hexdigest()})
    save_private(directory, "request.body", request["body"])
    connection_type = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
    connection = connection_type(parsed.hostname, parsed.port, timeout=timeout)
    try:
        target = parsed.path or "/"
        if parsed.query or "?" in request["url"]:
            target += "?" + parsed.query
        connection.putrequest(request["method"], target, skip_host=True, skip_accept_encoding=True)
        for name, value in headers:
            connection.putheader(name, value)
        connection.endheaders(request["body"])
        response = connection.getresponse()
        details = {"status": response.status, "reason": response.reason, "headers": metadata_headers(response.getheaders()), "complete": False}
        save_json(directory, "response.json", details)
        digest = hashlib.sha256()
        count = 0
        with os.fdopen(os.open(directory / "response.body", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as stream:
            while chunk := response.read(65536):
                stream.write(chunk)
                digest.update(chunk)
                count += len(chunk)
        if response.length not in (None, 0):
            raise CaptureError("response ended before its declared Content-Length")
        details.update(complete=True, body_bytes=count, body_sha256=digest.hexdigest())
        # This file belongs to this newly created private directory.
        (directory / "response.json").write_text(json.dumps(details, indent=2) + "\n", encoding="utf-8")
        return {"status": response.status, "body_bytes": count, "output": str(directory)}
    except (OSError, http.client.HTTPException, CaptureError) as exc:
        # Exception messages can contain target URLs or credentials. Persist only type.
        save_json(directory, "error.json", {"error": type(exc).__name__, "complete": False})
        raise CaptureError("request failed; partial evidence retained in output directory") from exc
    finally:
        connection.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("inspect", "replay"):
        sub = subparsers.add_parser(command)
        sub.add_argument("input", help="HAR or a raw HTTP/1 request file")
        sub.add_argument("--base-url", help="explicit origin for origin-form raw HTTP input")
        if command == "replay":
            sub.add_argument("--index", type=int, required=True, help="one-based request index from inspect")
            sub.add_argument("--output", required=True, help="new private evidence directory")
            sub.add_argument("--headers", help="JSON header overrides; null removes a header")
            sub.add_argument("--remove-header", action="append", default=[])
            sub.add_argument("--timeout", type=float, default=30)
    args = parser.parse_args(argv)
    try:
        entries, parse = load_capture(args.input, args.base_url)
        if args.command == "inspect":
            result = []
            for index, entry in enumerate(entries, 1):
                try:
                    result.append(describe(parse(entry), index))
                except CaptureError as exc:
                    result.append({"index": index, "replayable": False, "error": str(exc)})
        else:
            if args.index < 1 or args.index > len(entries):
                raise CaptureError("request index out of range")
            request = override_headers(parse(entries[args.index - 1]), args.headers, args.remove_header)
            result = replay(request, args.output, args.timeout)
        print(json.dumps(result, ensure_ascii=True, indent=2))
        return 0
    except (CaptureError, OSError) as exc:
        print("pwn-http: " + (str(exc) if isinstance(exc, CaptureError) else type(exc).__name__), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
