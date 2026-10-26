#!/usr/bin/env python3
"""Exercise the Docker deployment using isolated, disposable resources."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import uuid


def run(args, *, env=None, cwd=None, check=True, input=None):
    result = subprocess.run(
        [str(arg) for arg in args], env=env, cwd=cwd, input=input,
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120,
    )
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stdout}{result.stderr}")
    return result


def main():
    root = Path(__file__).resolve().parent.parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=root / "bin/crease")
    args = parser.parse_args()
    binary = args.binary.resolve()
    project = "crease-smoke-" + uuid.uuid4().hex[:12]
    backup_volume = project + "-backup"
    restore_volume = project + "-restore"

    with tempfile.TemporaryDirectory(prefix="crease-docker-smoke-") as temporary:
        work = Path(temporary)
        env = os.environ.copy()
        env.update(CREASE_OWNER_KEY=str(work / "owner.pub"), CREASE_PORT="0",
                   CREASE_BIND="127.0.0.1", CREASE_AUTHOR="Docker smoke test")
        # Do not inherit client identity or host-trust overrides from the caller.
        env.pop("CREASE_KEY", None)
        env.pop("CREASE_TRUST_HOST", None)
        env.pop("CREASE_KNOWN_HOSTS", None)
        compose = ["docker", "compose", "-p", project, "-f", root / "compose.yaml"]

        def dc(*parts, check=True):
            return run([*compose, *parts], env=env, check=check)

        def client(*parts, cwd=work, key="owner", check=True):
            return run([binary, "--key", work / key, "--known-hosts",
                        work / "known_hosts", *parts], cwd=cwd, env=env, check=check)

        def connection():
            container = dc("ps", "-q", "crease").stdout.strip()
            details = json.loads(run(["docker", "inspect", container]).stdout)[0]
            assert details["Config"]["User"] == "65532:65532"
            assert details["HostConfig"]["ReadonlyRootfs"]
            assert details["HostConfig"]["CapDrop"] == ["ALL"]
            port = details["NetworkSettings"]["Ports"]["2222/tcp"][0]["HostPort"]
            for _ in range(50):
                logs = dc("logs", "--no-color", "crease").stdout
                match = re.search(r"Host key: (SHA256:\S+)", logs)
                if match:
                    return port, match.group(1)
                time.sleep(0.1)
            raise RuntimeError("server did not report its host key")

        try:
            for key in ("owner", "stranger"):
                run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", work / key])
                (work / (key + ".pub")).chmod(0o644)
            dc("up", "-d", "--no-build")
            port, fingerprint = connection()

            def trust(port):
                scanned = run(["ssh-keyscan", "-T", "5", "-t", "ed25519",
                               "-p", port, "127.0.0.1"]).stdout
                actual = run(["ssh-keygen", "-lf", "-", "-E", "sha256"], input=scanned)
                assert actual.stdout.split()[1] == fingerprint, "host fingerprint mismatch"
                (work / "known_hosts").write_text(scanned)

            trust(port)
            url = f"ssh://127.0.0.1:{port}/smoke"
            source = work / "source"
            client("create", url, source)
            (source / ".env").write_text("SMOKE_TEST=container-persistence\n")
            (source / "README.md").write_text("Docker smoke test\n")
            client("add", ".", cwd=source)
            client("commit", "-m", "Verify container storage", cwd=source)
            client("push", cwd=source)
            client("clone", url, work / "clone")
            assert (work / "clone/.env").read_bytes() == (source / ".env").read_bytes()
            denied = client("clone", url, work / "denied", key="stranger", check=False)
            assert denied.returncode != 0, "unauthorized identity was accepted"
            print("PASS: non-root, read-only root, SSH authentication, create/commit/push/clone", flush=True)

            dc("stop", "crease")
            container = dc("ps", "-a", "-q", "crease").stdout.strip()
            state = json.loads(run(["docker", "inspect", container]).stdout)[0]["State"]
            assert state["ExitCode"] == 0 and not state["OOMKilled"], "unclean shutdown"
            dc("up", "-d", "--no-build", "--force-recreate")
            port, restarted_fingerprint = connection()
            assert restarted_fingerprint == fingerprint, "host identity changed"
            trust(port)
            client("clone", f"ssh://127.0.0.1:{port}/smoke", work / "after-recreate")
            assert (work / "after-recreate/.env").read_bytes() == (source / ".env").read_bytes()
            print("PASS: graceful stop, repository and host-key persistence after replacement", flush=True)

            dc("stop", "crease")
            for volume in (backup_volume, restore_volume):
                run(["docker", "volume", "create", volume])
            run(["docker", "run", "--rm", "-v", f"{project}_crease-data:/source",
                 "-v", f"{backup_volume}:/var/lib/crease", "crease:local", "backup",
                 "--data", "/source", "--output", "/var/lib/crease/snapshot"])
            run(["docker", "run", "--rm", "-v", f"{backup_volume}:/backup:ro",
                 "-v", f"{restore_volume}:/var/lib/crease", "crease:local", "restore-server",
                 "--input", "/backup/snapshot", "--data", "/var/lib/crease/restored"])
            print("PASS: offline backup and verified restore", flush=True)
        finally:
            dc("down", "-v", "--remove-orphans", check=False)
            for volume in (backup_volume, restore_volume):
                run(["docker", "volume", "rm", volume], check=False)


if __name__ == "__main__":
    main()
