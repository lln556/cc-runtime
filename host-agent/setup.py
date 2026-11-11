#!/usr/bin/env python3
"""Prepare the host-agent installation; enabling/restarting is a separate action."""
import json
import os
from pathlib import Path
import plistlib
import sys

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "src"))
from bridge import Worker, config


def main():
    local = ROOT / "config/local.json"
    if not local.exists():
        local.write_text((ROOT / "config/example.json").read_text())
        local.chmod(0o600)
    settings = config(local)
    worker = Worker(settings)
    worker.prepare()
    executable = ROOT / "bin/host-agent"
    executable.chmod(0o755)
    label = "local.cc-runtime.host-agent"
    plist = worker.private / (label + ".plist")
    data = {
        "Label": label,
        "ProgramArguments": [sys.executable, str(ROOT / "src/bridge.py"),
                             "--config", str(local), "serve"],
        "WorkingDirectory": str(ROOT.parent),
        "RunAtLoad": True,
        "KeepAlive": True,
        "ThrottleInterval": 15,
        "StandardOutPath": str(worker.private / "worker.stdout.log"),
        "StandardErrorPath": str(worker.private / "worker.stderr.log"),
        "EnvironmentVariables": {
            "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/sbin:/sbin"),
        },
    }
    with plist.open("wb") as stream:
        plistlib.dump(data, stream)
    plist.chmod(0o600)
    print(json.dumps({"config": str(local), "launch_agent_draft": str(plist),
                      "enabled": False, "container_restarted": False}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
