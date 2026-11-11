"""Opt-in lifecycle regression: CC_RUNTIME_DOCKER_TEST=1 python3 -m unittest discover -s tests -p test_network.py."""
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]

@unittest.skipUnless(os.environ.get("CC_RUNTIME_DOCKER_TEST") == "1", "opt-in Docker integration")
class NetworkLifecycleTests(unittest.TestCase):
    def test_restart_and_route_repair(self):
        name = "cc-net-test-" + uuid.uuid4().hex[:8]
        helper, dev = name + "-net", name + "-dev"
        image = "metacubex/mihomo:v1.19.19"
        def docker(*args):
            return subprocess.check_output(["docker", *args], text=True, stderr=subprocess.STDOUT).strip()
        def healthy():
            for _ in range(20):
                try:
                    docker("exec", helper, "sh", "/network.sh", "--check")
                    return
                except subprocess.CalledProcessError:
                    time.sleep(0.5)
            self.fail("Network did not recover within 10 seconds")
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "network.sh"
            script.write_text((ROOT / "scripts/containers/dev-network-init.sh").read_text().replace("172.30.0.2", "172.29.241.2"))
            try:
                docker("network", "create", "--subnet", "172.29.241.0/24", name)
                docker("run", "-d", "--name", helper, "--network", name,
                       "--cap-add", "NET_ADMIN", "-v", str(script) + ":/network.sh:ro",
                       "--entrypoint", "sh", image, "/network.sh")
                healthy()
                docker("run", "-d", "--name", dev, "--network", "container:" + helper,
                       "--entrypoint", "sleep", image, "300")
                docker("restart", dev)
                healthy()
                self.assertEqual(docker("exec", dev, "readlink", "/proc/self/ns/net"),
                                 docker("exec", helper, "readlink", "/proc/self/ns/net"))
                docker("exec", helper, "ip", "route", "replace", "default", "via", "172.29.241.1")
                docker("exec", helper, "iptables", "-D", "OUTPUT", "-d", "127.0.0.11/32", "-j", "REJECT")
                healthy()
                docker("stop", dev, helper)
                docker("start", helper)
                healthy()
                docker("start", dev)
                self.assertIn("default via 172.29.241.2", docker("exec", dev, "ip", "route"))
                healthy()
            finally:
                for container in [dev, helper]:
                    subprocess.run(["docker", "rm", "-f", container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                subprocess.run(["docker", "network", "rm", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
