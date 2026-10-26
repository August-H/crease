#!/usr/bin/env python3
"""Build local release artifacts without publishing anything."""
import hashlib
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
DEST = ROOT / "bin" / "releases"
DEST.mkdir(parents=True, exist_ok=True)
artifacts = []
for system in ("linux", "darwin"):
    for arch in ("amd64", "arm64"):
        output = DEST / f"crease-{system}-{arch}"
        subprocess.run(
            ["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(output), "./cmd/crease"],
            cwd=ROOT,
            env={**os.environ, "GOOS": system, "GOARCH": arch, "CGO_ENABLED": "0"},
            check=True,
        )
        artifacts.append(f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}")
(DEST / "SHA256SUMS").write_text("\n".join(artifacts) + "\n")
print(DEST)
