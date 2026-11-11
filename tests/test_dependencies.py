import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock
from unittest.mock import patch
import uuid

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("dev", ROOT / "scripts/dev.py")
dev = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dev)
RULES = json.loads((ROOT / "dependency-rules.json").read_text())


class HostAgentComposeTests(unittest.TestCase):
    def test_opt_in_overlay_preserves_dependency_mounts(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = root / "host-agent/config/enabled"
            marker.parent.mkdir(parents=True)
            with patch.object(dev, "ROOT", root), patch.object(dev, "BASE", root / "compose.yaml"), \
                    patch.object(dev, "OVERLAY", root / ".runtime/compose.dependencies.json"), \
                    patch.object(dev, "run") as run:
                dev.compose("config", "--quiet", overlay=True)
                self.assertNotIn(str(root / "host-agent/compose.yaml"), run.call_args.args[0])
                marker.touch()
                dev.compose("config", "--quiet", overlay=True)
                command = run.call_args.args[0]
                self.assertIn(str(root / ".runtime/compose.dependencies.json"), command)
                self.assertIn(str(root / "host-agent/compose.yaml"), command)
                self.assertEqual(command[-2:], ["config", "--quiet"])


class ScannerTests(unittest.TestCase):
    def test_generate_excludes_private_identity_storage(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "dependency-rules.json").write_text(json.dumps(RULES))
            for name in ("home", "host-keys", "tailscale-state"):
                directory = root / name
                directory.mkdir()
                (directory / "package.json").touch()
                # Scanning private state would encounter this invalid dependency mount.
                (directory / "node_modules").symlink_to("/does-not-exist")
            model = {"name": "test", "services": {
                "dev": {"volumes": [
                    {"type": "bind", "source": str(root), "target": dev.WORKSPACE_TARGET},
                    {"type": "bind", "source": str(root / "home"), "target": "/home/dev"},
                    {"type": "bind", "source": str(root / "host-keys"),
                     "target": "/var/lib/cc-runtime/ssh"}]},
                "tailscale": {"volumes": [{"type": "bind", "source": str(root / "tailscale-state"),
                                             "target": "/var/lib/tailscale"}]}}}
            with patch.object(dev, "ROOT", root), patch.object(dev, "STATE", root / ".runtime"), \
                    patch.object(dev, "OVERLAY", root / ".runtime/compose.json"), \
                    patch.object(dev, "compose", return_value=json.dumps(model)), \
                    patch.object(dev, "run", return_value="aarch64"):
                _, plan = dev.generate()
            self.assertEqual(plan["entries"], [])

    def test_project_detection_preserves_source_directories(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            files = ["js/package.json", "js/build/icon.png", "rust/Cargo.toml",
                     "rust/src/bin/main.rs", "java/pom.xml", "python/pyproject.toml",
                     "python/.vnev/pyvenv.cfg", "python/.vnev/lib/package.json",
                     "python/__pycache__/cached.pyc", "go/internal/build/build.go",
                     "vendor/source.js", "nested/node_modules/dependency/package.json"]
            for file in files:
                path = root / file
                path.parent.mkdir(parents=True, exist_ok=True)
                path.touch()
            entries, _ = dev.scan(root, RULES)
            paths = {entry["path"] for entry in entries}
            self.assertEqual(paths, {"js/node_modules", "rust/target", "java/target",
                                     "python/.venv", "python/.vnev", "nested/node_modules"})

    def test_explicit_exclusion_and_parent_mount(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "archive").mkdir()
            (root / "archive/package.json").touch()
            (root / "app").mkdir()
            (root / "app/package.json").touch()
            rules = copy.deepcopy(RULES)
            rules["exclude_paths"] = ["archive"]
            rules["include_paths"] = ["app", "app/node_modules"]
            entries, _ = dev.scan(root, rules)
            self.assertEqual([entry["path"] for entry in entries], ["app"])

    def test_refuses_symlink_and_path_escape(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "package.json").touch()
            (root / "node_modules").symlink_to("/tmp", target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "symlink"):
                dev.scan(root, RULES)
            for path in ["../outside", "/outside", "."]:
                with self.assertRaises(ValueError):
                    dev.relative_path(path)

    def test_stable_paths_and_architecture_isolation(self):
        entries = [{"path": "app/node_modules", "reason": "test"}]
        _, one = dev.make_plan(Path("/workspace"), copy.deepcopy(entries), "test", "aarch64")
        _, two = dev.make_plan(Path("/workspace"), copy.deepcopy(entries), "test", "aarch64")
        _, other = dev.make_plan(Path("/workspace"), copy.deepcopy(entries), "test", "x86_64")
        self.assertEqual(one["entries"], two["entries"])
        self.assertNotEqual(one["entries"][0]["subpath"], other["entries"][0]["subpath"])
        self.assertEqual(one["volume"], other["volume"])


class MultiWorkspaceTests(unittest.TestCase):
    def test_generate_keeps_namespaces_and_single_cache(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            company, personal = root / "company", root / "personal"
            for workspace in (company, personal):
                (workspace / "app").mkdir(parents=True)
                (workspace / "app/package.json").write_text("{}")
            model = {"name": "test", "services": {"dev": {"volumes": [
                {"type": "bind", "source": str(company), "target": dev.WORKSPACE_TARGET},
                {"type": "bind", "source": str(personal), "target": dev.PERSONAL_TARGET}]}}}
            state = root / "state"
            with mock.patch.object(dev, "compose", return_value=json.dumps(model)), \
                 mock.patch.object(dev, "run", return_value="aarch64"), \
                 mock.patch.object(dev, "STATE", state), \
                 mock.patch.object(dev, "OVERLAY", state / "overlay.json"):
                _, plan = dev.generate()
                overlay = json.loads((state / "overlay.json").read_text())
                volumes = overlay["services"]["dev"]["volumes"]
                self.assertEqual({m["target"] for m in volumes}, {
                    dev.WORKSPACE_TARGET + "/app/node_modules",
                    dev.PERSONAL_TARGET + "/app/node_modules", "/home/dev/.cache"})
                self.assertEqual(len(plan["entries"]), 2)
                self.assertEqual(len({e["subpath"] for e in plan["entries"]}), 2)
                self.assertEqual([len(w["entries"]) for w in plan["workspaces"]], [1, 1])
                _, original = dev.make_plan(company, [{"path": "app/node_modules"}], "test", "aarch64")
                self.assertEqual(original["cache_subpath"], plan["cache_subpath"])
                self.assertEqual(original["entries"][0]["subpath"], plan["entries"][0]["subpath"])
                # Reject mounting the same host tree under a second alias.
                model["services"]["dev"]["volumes"][1]["source"] = str(company)
                with mock.patch.object(dev, "compose", return_value=json.dumps(model)):
                    with self.assertRaisesRegex(ValueError, "overlap"):
                        dev.generate()


@unittest.skipUnless(os.environ.get("CC_RUNTIME_DOCKER_TEST") == "1", "opt-in Docker integration")
class DockerTests(unittest.TestCase):
    def test_home_seed_and_nested_workspace_mount(self):
        image = "cc-home-test-" + uuid.uuid4().hex[:10]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            workspace = root / "workspace"
            home = workspace / "runtime/dev-home"
            home.mkdir(parents=True)
            (workspace / "shared.txt").write_text("source")
            (workspace / "shared.txt").chmod(0o666)
            (home / ".zshrc").write_text("keep-user-config")
            (root / "Dockerfile").write_text('''FROM python:3.12-slim
RUN mkdir -p /home/dev/.local/bin /home/dev/.cache /home/dev/workspace && \\
    echo image-config > /home/dev/.zshrc && \\
    echo image-tool > /home/dev/.local/bin/tool && \\
    ln -s tool /home/dev/.local/bin/tool-link && \\
    echo no-copy > /home/dev/.cache/image-cache && \\
    echo no-copy > /home/dev/workspace/image-source && \\
    chown -R 1234:1234 /home/dev
''')
            try:
                subprocess.run(["docker", "build", "-q", "-t", image, str(root)],
                               check=True, stdout=subprocess.DEVNULL)
                dev.initialize_home(image, home, 1234, 1234)
                self.assertEqual((home / ".zshrc").read_text(), "keep-user-config")
                self.assertEqual((home / ".local/bin/tool").read_text().strip(), "image-tool")
                self.assertTrue((home / ".local/bin/tool-link").is_symlink())
                self.assertFalse((home / ".cache/image-cache").exists())
                self.assertFalse((home / "workspace/image-source").exists())
                (home / ".local/bin/tool").write_text("user-updated-tool")
                dev.initialize_home(image, home, 1234, 1234)
                self.assertEqual((home / ".local/bin/tool").read_text(), "user-updated-tool")
                code = '''from pathlib import Path
h=Path('/home/dev')
(h/'settings.txt').write_text('persistent-home')
(h/'workspace/shared.txt').write_text('shared-edit')
alias=h/'workspace/runtime/dev-home'
assert (alias/'settings.txt').read_text()=='persistent-home'
assert list((alias/'workspace').iterdir())==[]
'''
                subprocess.run(["docker", "run", "--rm", "--network", "none", "--user", "1234:1234",
                                "--mount", f"type=bind,source={home},target=/home/dev",
                                "--mount", f"type=bind,source={workspace},target=/home/dev/workspace",
                                image, "python", "-c", code], check=True)
                self.assertEqual((home / "settings.txt").read_text(), "persistent-home")
                self.assertEqual((workspace / "shared.txt").read_text(), "shared-edit")
            finally:
                subprocess.run(["docker", "image", "rm", image], stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL)

    def test_one_volume_subpaths_persistence_and_python_cache(self):
        project = "cc-deps-test-" + uuid.uuid4().hex[:10]
        volume = project + "_dependencies"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            workspace = root / "workspace"
            workspace.mkdir()
            app = workspace / "project $one"
            app.mkdir()
            for name in ["node_modules", ".venv", "target"]:
                (app / name).mkdir()
                (app / name / "host-only").write_text("mac-dependency")
            source = app / "shared.txt"
            source.write_text("host-source")
            source.chmod(0o666)
            (app / "sample.py").write_text("value = 42\n")
            (app / "__pycache__").mkdir()
            (app / "__pycache__/host-marker").write_text("unchanged")
            entries = [{"path": "project $one/" + name, "reason": "test"}
                       for name in ["node_modules", ".venv", "target"]]
            overlay, plan = dev.make_plan(workspace, entries, project, "test-architecture")
            base = {"services": {"dev": {"image": "python:3.12-slim", "user": "1234:1234",
                    "network_mode": "none", "command": ["python", "-c", "import time; time.sleep(300)"],
                    "volumes": [{"type": "bind", "source": str(workspace),
                                 "target": dev.WORKSPACE_TARGET}]}}}
            (root / "base.json").write_text(json.dumps(base))
            (root / "overlay.json").write_text(json.dumps(overlay).replace("$", "$$"))
            command = ["docker", "compose", "-p", project, "-f", str(root / "base.json"),
                       "-f", str(root / "overlay.json")]
            def compose(*args):
                return subprocess.run(command + list(args), check=True, text=True,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout
            try:
                dev.initialize(plan, 1234, 1234)
                compose("up", "-d")
                code = '''from pathlib import Path
import sys
p = Path('/home/dev/workspace/project $one')
for name in ['node_modules', '.venv', 'target']:
    assert not (p/name/'host-only').exists()
    (p/name/'linux-only').write_text(name)
(p/'shared.txt').write_text('container-source')
sys.path.insert(0, str(p))
import sample
assert sample.value == 42
assert sample.__cached__.startswith('/home/dev/.cache/python/pycache/')
assert Path(sample.__cached__).is_file()
'''
                compose("exec", "-T", "dev", "python", "-c", code)
                self.assertEqual(source.read_text(), "container-source")
                for name in ["node_modules", ".venv", "target"]:
                    self.assertEqual((app / name / "host-only").read_text(), "mac-dependency")
                    self.assertFalse((app / name / "linux-only").exists())
                self.assertEqual(list((app / "__pycache__").iterdir()), [app / "__pycache__/host-marker"])
                compose("up", "-d", "--force-recreate")
                compose("exec", "-T", "dev", "python", "-c",
                        "from pathlib import Path; p=Path('/home/dev/workspace/project $one'); "
                        "assert all((p/n/'linux-only').read_text()==n for n in ['node_modules','.venv','target'])")
                inspected = json.loads(subprocess.check_output(
                    ["docker", "inspect", compose("ps", "-q", "dev").strip()], text=True))
                volumes = {m["Name"] for m in inspected[0]["Mounts"] if m["Type"] == "volume"}
                self.assertEqual(volumes, {volume})
                compose("down", "-v")
                subprocess.run(["docker", "volume", "inspect", volume], check=True, stdout=subprocess.DEVNULL)
            finally:
                subprocess.run(command + ["down", "-v"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    unittest.main()
