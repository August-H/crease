#!/usr/bin/env python3
"""Compare local commands on identical disposable Crease and Git workspaces."""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import platform
import statistics
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="bin/crease")
    parser.add_argument("--files", type=int, default=1000)
    parser.add_argument("--history", type=int, default=10)
    parser.add_argument("--runs", type=int, default=7)
    parser.add_argument("--warmup-seconds", type=float, default=1.1,
                        help="age timestamps before warm status samples")
    parser.add_argument("--monitor", action="store_true", help="enable Crease's native filesystem monitor")
    parser.add_argument("--git-fsmonitor", action="store_true", help="enable Git's built-in filesystem monitor")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if min(args.files, args.history, args.runs) < 1 or args.warmup_seconds < 0:
        parser.error("files, history, and runs must be positive; warmup must be nonnegative")
    binary = str(Path(args.binary).resolve())
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, CREASE_AUTHOR="Benchmark")

    def run(cwd, command):
        start = time.perf_counter()
        result = subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(f"{command!r}: {result.stderr}")
        return time.perf_counter() - start

    def distribution(samples):
        return {"median_ms": round(statistics.median(samples) * 1000, 3),
                "max_ms": round(max(samples) * 1000, 3),
                "samples_ms": [round(x * 1000, 3) for x in samples]}

    result = {"timestamp_utc": datetime.now(timezone.utc).isoformat(),
              "platform": platform.platform(), "processor": platform.machine(),
              "go": subprocess.check_output(["go", "version"], text=True).strip(),
              "git": subprocess.check_output(["git", "--version"], text=True).strip(),
              "files": args.files, "history": args.history, "runs": args.runs,
              "warmup_seconds": args.warmup_seconds,
              "crease_monitor": args.monitor, "git_fsmonitor": args.git_fsmonitor,
              "scope": "local commands; global Git configuration disabled; warm OS caches; no network; monitor flags explicit",
              "results": {}}
    with tempfile.TemporaryDirectory(prefix="crease-benchmark-") as root:
        for tool in ("crease", "git"):
            directory = Path(root) / tool
            directory.mkdir()
            cmd = [binary] if tool == "crease" else ["git"]
            run(directory, cmd + (["init"] if tool == "crease" else ["init", "--initial-branch=main"]))
            if tool == "git":
                for key, value in (("user.name", "Benchmark"), ("user.email", "bench@example.invalid"),
                                   ("commit.gpgsign", "false"), ("core.hooksPath", os.devnull)):
                    run(directory, cmd + ["config", key, value])
            for i in range(args.files):
                file = directory / "src" / f"group-{i // 1000:04d}" / f"file-{i:06d}.txt"
                file.parent.mkdir(parents=True, exist_ok=True)
                file.write_text(f"file {i}\nsecond line\nthird line\n")
            timings = {"track_ms": round(run(directory, cmd + ["add", "."]) * 1000, 3)}
            timings["initial_commit_ms"] = round(run(directory, cmd + ["commit", "-m", "Initial"]) * 1000, 3)
            stop = None
            try:
                if tool == "crease" and args.monitor:
                    stop = cmd + ["monitor", "stop"]
                    timings["monitor_start_ms"] = round(run(directory, cmd + ["monitor", "start"]) * 1000, 3)
                elif tool == "git" and args.git_fsmonitor:
                    run(directory, cmd + ["config", "core.fsmonitor", "true"])
                    stop = cmd + ["fsmonitor--daemon", "stop"]
                    timings["monitor_start_ms"] = round(run(directory, cmd + ["fsmonitor--daemon", "start"]) * 1000, 3)
                status = cmd + (["status"] if tool == "crease" else ["status", "--porcelain=v1"])
                timings["status_first_ms"] = round(run(directory, status) * 1000, 3)
                time.sleep(args.warmup_seconds)
                timings["status_warm"] = distribution([run(directory, status) for _ in range(args.runs)])
                changed = directory / "src" / "group-0000" / "file-000000.txt"
                commits, diffs, workflows = [], [], []
                for i in range(args.history):
                    changed.write_text(f"revision {i}\nsecond line\nthird line\n")
                    diffs.append(run(directory, cmd + ["diff"]))
                    stage_time = run(directory, cmd + ["add", "."]) if tool == "git" else 0
                    commit_time = run(directory, cmd + ["commit", "-m", f"Revision {i}"])
                    commits.append(commit_time)
                    workflows.append(stage_time + commit_time)
                timings["diff_one_file"] = distribution(diffs)
                timings["commit_one_file"] = distribution(commits)
                timings["commit_including_tracking"] = distribution(workflows)
                log = cmd + (["log", "--limit", "20"] if tool == "crease" else ["log", "-20"])
                timings["log_warm"] = distribution([run(directory, log) for _ in range(args.runs)])
                meta = directory / (".crease" if tool == "crease" else ".git")
                timings["metadata_bytes"] = sum(p.stat().st_size for p in meta.rglob("*") if p.is_file())
                timings["commit_scope"] = "all tracked edits" if tool == "crease" else "index already updated; staging time excluded"
                result["results"][tool] = timings
            finally:
                if stop:
                    stopped = subprocess.run(stop, cwd=directory, env=env, capture_output=True, text=True, timeout=10)
                    if stopped.returncode:
                        raise RuntimeError(f"monitor cleanup failed for {tool}: {stopped.stderr}")
    text = json.dumps(result, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(text)
    print(text, end="")


if __name__ == "__main__":
    main()
