"""Exercise command routing without starting Docker or contacting a model."""

import fnmatch
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
STUB = """#!/usr/bin/env python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
record = {
    "tool": name, "args": sys.argv[1:], "cwd": os.getcwd(),
    "controls": {key: value for key, value in os.environ.items() if key.startswith("PWNMESH_")},
    "has_model_credentials": bool(os.environ.get("ANTHROPIC_AUTH_TOKEN") or os.environ.get("OPENAI_API_KEY")),
    "cgo": os.environ.get("CGO_ENABLED"),
}
with open(os.environ["COMMAND_RECORD"], "a") as out:
    out.write(json.dumps(record) + "\\n")
if name == "docker":
    if os.environ.get("FAKE_DOCKER_FAILURE") in sys.argv[1:]:
        sys.exit(1)
    if "info" in sys.argv:
        print("linux")
"""


@unittest.skipUnless(sys.platform.startswith("linux"), "development commands target Linux")
class CommandTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = pathlib.Path(self.temp.name)
        self.record = self.directory / "commands.jsonl"
        for tool in ("go", "docker", "npm"):
            path = self.directory / tool
            path.write_text(STUB)
            path.chmod(0o755)
        self.env = {
            key: value for key, value in os.environ.items()
            if not key.startswith(("PWNMESH_", "ANTHROPIC_", "OPENAI_"))
        }
        self.env.update(PATH=str(self.directory) + os.pathsep + self.env["PATH"], COMMAND_RECORD=str(self.record))

    def run_script(self, name, *args, success=True, root=ROOT):
        result = subprocess.run(
            ["bash", str(root / "scripts" / name), *args], cwd=self.directory,
            env=self.env, capture_output=True, text=True, timeout=10,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return [json.loads(line) for line in self.record.read_text().splitlines()] if self.record.exists() else []

    def inherited_controls(self):
        self.env.update(
            PWNMESH_DOCKER_TEST_IMAGE="fixture-worker",
            PWNMESH_LIVE_READINESS_TEST="1",
            PWNMESH_LIVE_CONTENTION_TEST="1",
            PWNMESH_ASSET_GRAPH_REPLAY_DIR="/retained",
            PWNMESH_RELEASE_AUDIT_RECOVERY_DIR="/retained",
            PWNMESH_WORKER_GRAPH_TIMING_OUTPUT="/timing.json",
            ANTHROPIC_AUTH_TOKEN="test-token",
            OPENAI_API_KEY="test-token",
        )

    def model_environment(self):
        self.env.update(
            ANTHROPIC_BASE_URL="https://model.example.invalid",
            ANTHROPIC_AUTH_TOKEN="test-token",
            ANTHROPIC_DEFAULT_FABLE_MODEL="test-model",
            PWNMESH_LIVE_OUTPUT="/test-output",
        )

    def test_offline_keeps_all_oracles_without_inherited_opt_ins(self):
        self.inherited_controls()
        self.env["PWNMESH_MODEL_PROTOCOL"] = "openai"
        records = self.run_script("test-integration.sh", "offline")
        self.assertEqual([record["tool"] for record in records], ["npm", "go"])
        run = records[-1]
        self.assertEqual(run["tool"], "go")
        self.assertEqual(run["cwd"], str(ROOT))
        self.assertEqual(run["args"], ["test", "-mod=readonly", "-race", "-count=1", "./tests/integration"])
        self.assertEqual(run["controls"], {})
        self.assertFalse(run["has_model_credentials"])

    def test_ci_go_suite_also_isolates_acceptance(self):
        self.inherited_controls()
        records = self.run_script("check.sh", "go")
        self.assertEqual([record["tool"] for record in records], ["npm", "go", "go"])
        self.assertEqual(records[0]["args"], ["--prefix", "web", "run", "build"])
        self.assertEqual([record["args"][0] for record in records[1:]], ["test", "vet"])
        self.assertEqual(records[1]["args"], ["test", "-mod=readonly", "-race", "-count=1", "-timeout=15m", "./..."])
        for record in records:
            self.assertEqual(record["controls"], {})
            self.assertFalse(record["has_model_credentials"])
        for record in records[1:]:
            self.assertEqual(record["args"][-1], "./...")

    def test_build_routes_an_explicit_output_path_and_reproducible_flags(self):
        output = self.directory / "build output" / "pwnmesh"
        records = self.run_script("build.sh", str(output))
        self.assertTrue(output.parent.is_dir())
        self.assertEqual([record["tool"] for record in records], ["npm", "go"])
        self.assertEqual(records[0]["args"], ["--prefix", "web", "run", "build"])
        self.assertEqual(records[-1]["args"], ["build", "-mod=readonly", "-trimpath", "-ldflags=-s -w", "-o", str(output), "./cmd/pwnmesh"])
        self.assertEqual(records[-1]["cgo"], "0")

    def test_web_suite_builds_and_checks_without_go(self):
        self.inherited_controls()
        records = self.run_script("check.sh", "web")
        self.assertEqual([record["tool"] for record in records], ["npm", "npm"])
        self.assertEqual([record["args"] for record in records], [["--prefix", "web", "run", "build"], ["--prefix", "web", "test"]])
        self.assertTrue(all(not record["has_model_credentials"] for record in records))

    def test_prebuilt_requires_nonempty_entry_and_javascript_before_go(self):
        workspace = self.directory / "prebuilt"
        (workspace / "scripts").mkdir(parents=True)
        for name in ("build.sh", "common.sh"):
            shutil.copyfile(ROOT / "scripts" / name, workspace / "scripts" / name)
        self.env["PWNMESH_WEB_PREBUILT"] = "1"
        self.assertEqual(self.run_script("build.sh", root=workspace, success=False), [])
        (workspace / "web/dist/assets").mkdir(parents=True)
        (workspace / "web/dist/index.html").write_text('<script src="/static/assets/index.js"></script>')
        self.assertEqual(self.run_script("build.sh", root=workspace, success=False), [])
        (workspace / "web/dist/assets/index.js").write_text('export {};')
        records = self.run_script("build.sh", root=workspace)
        self.assertEqual([record["tool"] for record in records], ["go"])
        self.assertEqual(records[0]["cwd"], str(workspace))

    def test_invalid_prebuilt_mode_fails_before_build_tools(self):
        self.env["PWNMESH_WEB_PREBUILT"] = "yes"
        self.assertEqual(self.run_script("build.sh", success=False), [])

    def test_docker_runs_both_packages_with_scripted_models_only(self):
        self.inherited_controls()
        records = self.run_script("test-integration.sh", "docker")
        self.assertEqual([record["tool"] for record in records], ["docker", "docker", "npm", "go"])
        self.assertEqual(records[0]["args"][:2], ["--host", "unix:///var/run/docker.sock"])
        self.assertIn("inspect", records[1]["args"])
        self.assertEqual(records[-1]["args"][-4:], ["-run", "^TestDocker", "./tests/integration", "./internal/docker"])
        for record in records:
            self.assertEqual(record["controls"], {"PWNMESH_DOCKER_TEST_IMAGE": "fixture-worker"})
            self.assertFalse(record["has_model_credentials"])

    def test_readiness_runs_one_live_entrypoint_without_docker(self):
        self.inherited_controls()
        self.model_environment()
        records = self.run_script("test-integration.sh", "live", "readiness")
        self.assertEqual([record["tool"] for record in records], ["npm", "go"])
        self.assertEqual(records[-1]["args"][-3:], ["-run", "^TestLiveModelReadiness$", "./tests/integration"])
        self.assertEqual(records[-1]["controls"], {"PWNMESH_LIVE_READINESS_TEST": "1", "PWNMESH_LIVE_OUTPUT": "/test-output"})
        self.assertTrue(records[-1]["has_model_credentials"])

    def test_docker_preflight_failure_never_starts_go_tests(self):
        self.env["PWNMESH_DOCKER_TEST_IMAGE"] = "fixture-worker"
        for failure in ("info", "inspect"):
            with self.subTest(failure=failure):
                self.env["FAKE_DOCKER_FAILURE"] = failure
                self.record.unlink(missing_ok=True)
                records = self.run_script("test-integration.sh", "docker", success=False)
                self.assertTrue(records)
                self.assertTrue(all(record["tool"] == "docker" for record in records))

    def test_external_suites_fail_before_go_when_requirements_are_missing(self):
        for args in (("docker",), ("live",), ("live", "unknown"), ("live", "readiness")):
            with self.subTest(args=args):
                self.assertEqual(self.run_script("test-integration.sh", *args, success=False), [])
        self.model_environment()
        self.assertEqual(self.run_script("test-integration.sh", "live", "contention", success=False), [])
        del self.env["PWNMESH_LIVE_OUTPUT"]
        self.assertEqual(self.run_script("test-integration.sh", "live", "readiness", success=False), [])

    def test_live_scenario_registry_covers_only_external_entrypoints(self):
        source = (ROOT / "tests/integration/live_acceptance_test.go").read_text()
        entrypoints = dict(re.findall(r'func (Test\w+)\(t \*testing.T\) \{\s+if os.Getenv\("(PWNMESH_LIVE_\w+_TEST)"\)', source))
        script = (ROOT / "scripts/test-integration.sh").read_text()
        scenarios = re.findall(r'\s+([\w-]+)\) test_name=(Test\w+); gate=(PWNMESH_LIVE_\w+_TEST) ;;', script)
        self.assertEqual(len(scenarios), len(entrypoints))
        self.assertEqual({name: gate for _, name, gate in scenarios}, entrypoints)
        for scenario, name, gate in scenarios:
            with self.subTest(scenario=scenario):
                self.model_environment()
                self.env["PWNMESH_DOCKER_TEST_IMAGE"] = "fixture-worker"
                self.record.unlink(missing_ok=True)
                records = self.run_script("test-integration.sh", "live", scenario)
                self.assertEqual(records[-1]["args"][-3:], ["-run", "^" + name + "$", "./tests/integration"])
                enabled = {key for key, value in records[-1]["controls"].items() if key.endswith("_TEST") and value == "1"}
                self.assertEqual(enabled, {gate})


class ContainerBuildTests(unittest.TestCase):
    def test_build_context_keeps_repository_check_inputs(self):
        rules = (ROOT / ".dockerignore").read_text().splitlines()
        checks = (ROOT / "internal/config/repository_test.go").read_text()
        inputs = re.findall(r'"((?:docs|container)/[^"\n]+\.md)"', checks)
        inputs += ["scripts/build.sh", "scripts/check.sh", "scripts/common.sh", "web/package.json", "web/package-lock.json", "web/index.html", "web/src/main.ts"]
        for name in inputs:
            excluded = False
            ancestors = [name, *(str(parent) for parent in pathlib.PurePosixPath(name).parents)]
            for rule in rules:
                if not rule or rule.startswith("#"):
                    continue
                include = rule.startswith("!")
                pattern = rule.lstrip("!").strip("/")
                if any(fnmatch.fnmatchcase(path, pattern) for path in ancestors):
                    excluded = not include
            self.assertFalse(excluded, f"release checks require {name} in the build context")

    def test_only_explicit_development_target_can_bypass_release_checks(self):
        source = (ROOT / "Dockerfile").read_text()
        stages, bodies = {}, {}
        current = None
        for line in source.splitlines():
            stage = re.match(r"FROM (\S+) AS (\S+)", line, re.I)
            if stage:
                parent, current = stage.groups()
                stages[current] = {parent}
                bodies[current] = []
            elif current:
                bodies[current].append(line)
                copied = re.match(r"COPY --from=(\S+)", line, re.I)
                if copied:
                    stages[current].add(copied.group(1))

        def dependencies(name):
            found, pending = set(), [name]
            while pending:
                item = pending.pop()
                if item not in found:
                    found.add(item)
                    pending.extend(stages.get(item, ()))
            return found

        self.assertEqual(current, "runtime", "plain docker build must select the release image")
        self.assertIn("test", dependencies(current))
        self.assertIn("build", dependencies(current))
        self.assertIn("web-build", dependencies(current))
        self.assertNotIn("test", dependencies("development"))
        self.assertIn("build", dependencies("development"))
        self.assertIn("scripts/check.sh go", "\n".join(bodies["test"]))
        self.assertIn("scripts/build.sh /out/pwnmesh", "\n".join(bodies["build"]))
        self.assertIn("npm ci", "\n".join(bodies["web-build"]))
        self.assertIn("npm run build && npm test", "\n".join(bodies["web-build"]))
        self.assertIn("COPY --from=web-build /web/dist ./web/dist", "\n".join(bodies["source"]))
        self.assertNotIn("node", "\n".join(bodies["runtime-base"]))

    def test_worker_browser_dependency_is_versioned(self):
        source = (ROOT / "container/Dockerfile").read_text()
        self.assertRegex(source, r"(?m)^ARG PLAYWRIGHT_CLI_VERSION=\d+\.\d+\.\d+$")
        self.assertIn("@playwright/cli@${PLAYWRIGHT_CLI_VERSION}", source)
        self.assertNotIn("@latest", source)

    def test_local_archive_is_excluded_from_source_and_build_context(self):
        self.assertIn("/.local/", (ROOT / ".gitignore").read_text().splitlines())
        self.assertIn(".local", (ROOT / ".dockerignore").read_text().splitlines())


if __name__ == "__main__":
    unittest.main()
