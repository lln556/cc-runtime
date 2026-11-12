#!/usr/bin/env python3
"""Scan bind-mounted projects and isolate Linux artifacts in one Docker volume."""
import argparse
import fnmatch
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
STATE = ROOT / ".runtime"
BASE = ROOT / "compose.yaml"
OVERLAY = STATE / "compose.dependencies.json"
WORKSPACE_TARGET = "/home/dev/workspace"
HELPER_IMAGE = "metacubex/mihomo:latest"


def run(args, *, capture=False, input=None):
    return subprocess.run(args, check=True, text=True, input=input,
                          stdout=subprocess.PIPE if capture else None).stdout


def compose(*args, overlay=False, capture=False):
    command = ["docker", "compose", "--project-directory", str(ROOT), "-f", str(BASE)]
    if overlay:
        command += ["-f", str(OVERLAY)]
    return run(command + list(args), capture=capture)


def relative_path(value):
    path = PurePosixPath(value)
    if not value or path.is_absolute() or ".." in path.parts or path == PurePosixPath("."):
        raise ValueError(f"Expected a nonempty workspace-relative path: {value!r}")
    return path.as_posix()


def scan(workspace, rules):
    """Never follow symlinks or descend into selected dependency roots."""
    workspace = workspace.resolve(strict=True)
    excludes = [relative_path(p) for p in rules["exclude_paths"]]
    selected = {}
    symlinks = []

    def excluded(rel):
        return any(rel == p or rel.startswith(p + "/") for p in excludes)

    def add(path, reason):
        rel = path.relative_to(workspace).as_posix()
        if excluded(rel):
            return
        # Refuse a mount through a symlink, including any ancestor.
        part = workspace
        for name in Path(rel).parts:
            part = part / name
            if part.is_symlink():
                raise ValueError(f"Dependency path is a symlink; resolve or exclude it first: {rel}")
        if path.exists() and not path.is_dir():
            raise ValueError(f"Dependency mount would cover a file: {rel}")
        selected[rel] = reason

    for rel in rules["include_paths"]:
        add(workspace / relative_path(rel), "explicit include_paths")

    def walk_error(error):
        raise error  # Do not silently produce an incomplete isolation plan.

    for base, dirs, files in os.walk(workspace, followlinks=False, onerror=walk_error):
        here = Path(base)
        rel = here.relative_to(workspace).as_posix()
        if excluded(rel) or rel in selected:
            dirs[:] = []
            continue
        if "pyvenv.cfg" in files:
            if here == workspace:
                raise ValueError("The workspace itself is a Python virtual environment")
            add(here, "pyvenv.cfg")
            dirs[:] = []
            continue
        for rule in rules["project_rules"]:
            matches = sorted({f for f in files for pattern in rule["markers"]
                              if fnmatch.fnmatchcase(f, pattern)})
            if matches:
                for name in rule["directories"]:
                    # Project rules select immediate children, not arbitrary paths.
                    if "/" in name or name in {"", ".", ".."}:
                        raise ValueError(f"Invalid project directory name: {name!r}")
                    add(here / name, "project: " + ", ".join(matches))
        keep = []
        for name in sorted(dirs):
            path = here / name
            child = path.relative_to(workspace).as_posix()
            if excluded(child) or name in rules["skip_directory_names"]:
                continue
            if name in rules["directory_names"]:
                add(path, "directory: " + name)
            if child in selected:
                continue
            if path.is_symlink():
                symlinks.append(child)
                continue
            keep.append(name)
        dirs[:] = keep
    # A selected parent already isolates everything underneath it.
    paths = []
    for rel in sorted(selected, key=lambda p: (len(PurePosixPath(p).parts), p)):
        if not any(rel.startswith(parent + "/") for parent in paths):
            paths.append(rel)
    return [{"path": p, "reason": selected[p]} for p in sorted(paths)], sorted(symlinks)


def make_plan(workspace, entries, project, architecture):
    identity = hashlib.sha256(str(workspace.resolve()).encode()).hexdigest()[:16]
    namespace = f"linux-{architecture}/{identity}"
    mounts = []
    for entry in entries:
        key = hashlib.sha256(entry["path"].encode()).hexdigest()
        entry["subpath"] = f"{namespace}/projects/{key}"
        mounts.append({"type": "volume", "source": "dependencies",
                       "target": WORKSPACE_TARGET + "/" + entry["path"],
                       "volume": {"nocopy": True, "subpath": entry["subpath"]}})
    cache = namespace + "/cache"
    mounts.append({"type": "volume", "source": "dependencies",
                   "target": "/home/dev/.cache", "volume": {"nocopy": True, "subpath": cache}})
    volume = project + "_dependencies"
    overlay = {"services": {"dev": {"volumes": mounts, "environment": {
        "PYTHONPYCACHEPREFIX": "/home/dev/.cache/python/pycache",
        "XDG_CACHE_HOME": "/home/dev/.cache"}}},
        "volumes": {"dependencies": {"external": True, "name": volume}}}
    plan = {"workspace": str(workspace), "volume": volume, "architecture": architecture,
            "entries": entries, "cache_subpath": cache}
    return overlay, plan


def generate():
    model = json.loads(compose("config", "--format", "json", capture=True))
    mounts = model["services"]["dev"]["volumes"]
    workspace_mount = next(m for m in mounts if m["target"] == WORKSPACE_TARGET)
    if workspace_mount["type"] != "bind":
        raise ValueError("dev workspace must be a bind mount")
    workspace = Path(workspace_mount["source"]).resolve(strict=True)
    rules = json.loads((ROOT / "dependency-rules.json").read_text())
    home_mount = next((m for m in mounts if m["target"] == "/home/dev"), None)
    if home_mount:
        if home_mount["type"] != "bind":
            raise ValueError("dev home must be a bind mount")
        home = Path(home_mount["source"]).resolve()
        if home == workspace or home in workspace.parents:
            raise ValueError("dev home must not contain the shared workspace")
        if workspace in home.parents:
            rules["exclude_paths"].append(home.relative_to(workspace).as_posix())
    # Identity directories can be root-only on Linux and contain no project dependencies.
    for service in model["services"].values():
        for mount in service.get("volumes", []):
            if mount["type"] == "bind" and mount["target"] in {
                    "/var/lib/cc-runtime/ssh", "/var/lib/tailscale"}:
                identity = Path(mount["source"]).resolve()
                if identity == workspace or identity in workspace.parents:
                    raise ValueError("Identity storage must not contain the shared workspace")
                if workspace in identity.parents:
                    rules["exclude_paths"].append(identity.relative_to(workspace).as_posix())
    # Exclude host screenshots path from dependency scan if located within workspace.
    screenshots_env = os.environ.get("HOST_SCREENSHOTS_PATH") or os.environ.get("CLEANSHOT_MEDIA_PATH")
    screenshots_target = None
    if screenshots_env and screenshots_env.strip():
        screenshots_path = Path(screenshots_env.strip()).expanduser()
        if screenshots_path.is_dir():
            resolved_screenshots = screenshots_path.resolve()
            screenshots_target = resolved_screenshots.as_posix()
            if resolved_screenshots == workspace or resolved_screenshots in workspace.parents:
                raise ValueError("Host screenshots path must not contain the shared workspace")
            if workspace in resolved_screenshots.parents:
                rules["exclude_paths"].append(resolved_screenshots.relative_to(workspace).as_posix())
    entries, symlinks = scan(workspace, rules)
    architecture = run(["docker", "info", "--format", "{{.Architecture}}"], capture=True).strip()
    if not architecture or not all(c.isalnum() or c in "_-" for c in architecture):
        raise ValueError("Unexpected Docker server architecture")
    overlay, plan = make_plan(workspace, entries, model["name"], architecture)
    plan["skipped_symlinks"] = symlinks
    if screenshots_target:
        overlay["services"]["dev"]["volumes"].append({
            "type": "bind",
            "source": screenshots_target,
            "target": screenshots_target,
            "read_only": True,
            "bind": {"create_host_path": False}
        })
        plan["host_screenshots_path"] = screenshots_target
    STATE.mkdir(exist_ok=True)
    # Compose interpolates '$' in values, even in JSON. Escape literal path characters.
    encoded = json.dumps(overlay, ensure_ascii=False, indent=2).replace("$", "$$")
    temporary = STATE / "compose.dependencies.tmp"
    temporary.write_text(encoded + "\n")
    temporary.replace(OVERLAY)
    (STATE / "dependencies.json").write_text(json.dumps(plan, ensure_ascii=False, indent=2) + "\n")
    compose("config", "--quiet", overlay=True)
    print(f"Scanned {workspace}: {len(entries)} isolated directories, one volume: {plan['volume']}", flush=True)
    if symlinks:
        print(f"Skipped {len(symlinks)} symlink directories; see .runtime/dependencies.json", flush=True)
    return model, plan


def initialize_home(image, home, uid, gid):
    """Seed the bind mount from the image once, without overwriting user files."""
    home.mkdir(parents=True, exist_ok=True)
    script = '''set -eu
if [ ! -f /persistent-home/.cc-runtime-home-initialized ]; then
    for path in /home/dev/.[!.]* /home/dev/..?* /home/dev/*; do
        [ -e "$path" ] || [ -L "$path" ] || continue
        case "${path##*/}" in workspace|.cache) continue ;; esac
        cp -a -n "$path" /persistent-home/
    done
    mkdir -p /persistent-home/workspace
    chown "$1:$2" /persistent-home /persistent-home/workspace
    touch /persistent-home/.cc-runtime-home-initialized
fi
'''
    run(["docker", "run", "--rm", "--network", "none", "--user", "0:0",
         "--mount", f"type=bind,source={home},target=/persistent-home",
         "--entrypoint", "/bin/sh", image, "-c", script, "init", str(uid), str(gid)])


def initialize(plan, uid, gid):
    volume = plan["volume"]
    # External volume intentionally survives `compose down -v`.
    run(["docker", "volume", "create", volume], capture=True)
    paths = [entry["subpath"] for entry in plan["entries"]] + [plan["cache_subpath"]]
    script = '''set -eu
while IFS= read -r path; do
    mkdir -p "/deps/$path"
    chown "$1:$2" "/deps/$path"
done
'''
    run(["docker", "run", "--rm", "-i", "--network", "none", "--user", "0:0",
         "--mount", f"type=volume,source={volume},target=/deps,volume-nocopy",
         "--entrypoint", "/bin/sh", HELPER_IMAGE, "-c", script, "init", str(uid), str(gid)],
        input="\n".join(paths) + "\n")


def initialize_identity(model, image):
    """Preserve the existing SSH identity before Compose replaces dev."""
    ssh_mount = next(m for m in model["services"]["dev"]["volumes"]
                     if m["target"] == "/var/lib/cc-runtime/ssh")
    if ssh_mount["type"] != "bind":
        raise ValueError("SSH host keys must use a bind mount")
    ssh_directory = Path(ssh_mount["source"]).resolve()
    ssh_directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    tailscale_mount = next(m for m in model["services"]["tailscale"]["volumes"]
                          if m["target"] == "/var/lib/tailscale")
    if tailscale_mount["type"] != "bind":
        raise ValueError("Tailscale state must use a bind mount")
    Path(tailscale_mount["source"]).mkdir(parents=True, exist_ok=True, mode=0o700)
    helper = ["docker", "run", "--rm", "--network", "none", "--user", "0:0",
              "--mount", f"type=bind,source={ssh_directory},target=/keys"]
    existing = run(helper + ["--entrypoint", "/bin/sh", image, "-c",
                            'find /keys -name "ssh_host_*_key" -print'], capture=True)
    if existing.strip():
        run(helper + ["--entrypoint", "/bin/sh", image, "-c",
                      'set -eu; for type in rsa ecdsa ed25519; do '
                      'ssh-keygen -y -f "/keys/ssh_host_${type}_key" > /dev/null; done'])
        return  # Never overwrite an established persistent identity.
    container = compose("ps", "--all", "--quiet", "dev", capture=True).strip()
    if not container:
        return  # First deployment: entrypoint generates fresh keys in the directory.
    if len(container.splitlines()) != 1:
        raise ValueError("Expected exactly one dev container for SSH identity migration")
    mounts = json.loads(run(["docker", "inspect", "--format", "{{json .Mounts}}", container],
                            capture=True))
    old_directory = ("/var/lib/cc-runtime/ssh" if any(
        m["Destination"] == "/var/lib/cc-runtime/ssh" for m in mounts) else "/etc/ssh")
    with tempfile.TemporaryDirectory(prefix="cc-ssh-migration-") as temporary:
        # docker cp also works with a stopped container; do not print key material.
        source = Path(temporary) / "ssh"
        run(["docker", "cp", f"{container}:{old_directory}", str(source)])
        if not all((source / f"ssh_host_{kind}_key").is_file()
                   for kind in ("rsa", "ecdsa", "ed25519")):
            raise ValueError("Existing dev host keys are incomplete; refusing to replace its identity")
        script = '''set -eu
umask 077
for type in rsa ecdsa ed25519; do
    ssh-keygen -y -f "/source/ssh_host_${type}_key" > /dev/null
done
chmod 700 /keys
for type in rsa ecdsa ed25519; do
    cp "/source/ssh_host_${type}_key" "/keys/ssh_host_${type}_key"
    chown root:root "/keys/ssh_host_${type}_key"
    chmod 600 "/keys/ssh_host_${type}_key"
done
'''
        run(helper + ["--mount", f"type=bind,source={source},target=/source,readonly",
                      "--entrypoint", "/bin/sh", image, "-c", script])
    print("Preserved existing dev SSH host keys in the host directory.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("scan", help="Generate and validate mount plan; do not start containers")
    sub.add_parser("prepare", help="Scan and initialize volume using a local dev image")
    up = sub.add_parser("up", help="Pull image, scan, initialize, and recreate services")
    up.add_argument("--no-pull", "--no-build", dest="no_pull", action="store_true",
                    help="Use the local image without pulling (--no-build is a legacy alias)")
    passthrough = sub.add_parser("compose", help="Run Compose with the generated mounts")
    passthrough.add_argument("args", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    STATE.mkdir(exist_ok=True)
    # macOS and Linux: prevent overlapping scans/startups from replacing each other's plan.
    import fcntl
    with (STATE / "lock").open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError("Another dev.py command is still running; wait for it to finish and retry.") from None
        if args.command == "compose":
            if not OVERLAY.exists():
                raise ValueError("Run 'python3 scripts/dev.py up' first")
            compose(*args.args, overlay=True)
            return
        model, plan = generate()
        if args.command == "scan":
            return
        if args.command == "up" and not args.no_pull:
            compose("pull", "dev")
        image = model["services"]["dev"].get("image", model["name"] + "-dev")
        try:
            run(["docker", "image", "inspect", "--format", "{{.Id}}", image], capture=True)
        except subprocess.CalledProcessError:
            raise ValueError(f"Local image {image!r} is missing. Run 'python3 scripts/dev.py up' "
                             f"to pull it, or tag your existing image as {image}.") from None
        ids = run(["docker", "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh",
                   image, "-c", "id -u dev; id -g dev"], capture=True).split()
        if len(ids) != 2 or not all(i.isdecimal() for i in ids):
            raise ValueError("Could not determine dev user UID/GID from the built image")
        home_mount = next((m for m in model["services"]["dev"]["volumes"]
                           if m["target"] == "/home/dev"), None)
        if home_mount:
            initialize_home(image, Path(home_mount["source"]).resolve(), *map(int, ids))
        initialize_identity(model, image)
        initialize(plan, *map(int, ids))
        print("Dependency subdirectories initialized; host dependencies were not copied.", flush=True)
        if args.command == "up":
            compose("up", "-d", "--force-recreate", overlay=True)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Error: {error}", file=sys.stderr)
        sys.exit(1)
