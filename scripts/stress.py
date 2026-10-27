#!/usr/bin/env python3
"""Verify and benchmark long histories and concurrent reviews against Git over SSH.

Requires Go, Git, OpenSSH, and Python 3.9+. All repositories, identities, host trust,
and server processes are disposable. No user repositories or services are used.
"""

import argparse
import bisect
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import random
import re
import selectors
import shlex
import shutil
import sqlite3
import statistics
import subprocess
import sys
import tempfile
import textwrap
import threading
import time


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def distribution(samples):
    ordered = sorted(samples)
    return {"count": len(samples), "median_ms": round(statistics.median(samples), 3),
            "p95_ms": round(ordered[math.ceil(len(samples) * .95) - 1], 3),
            "max_ms": round(max(samples), 3), "total_ms": round(sum(samples), 3),
            "samples_ms": [round(value, 3) for value in samples]}


def disk_usage(directory):
    files = [p for p in directory.rglob("*") if p.is_file()]
    return {"logical_bytes": sum(p.stat().st_size for p in files),
            "allocated_bytes": sum(p.stat().st_blocks * 512 for p in files),
            "files": len(files), "packs": sum(p.suffix == ".pack" for p in files)}


def manifest(directory):
    result = {}
    for parent, dirs, files in os.walk(directory):
        dirs[:] = sorted(d for d in dirs if d not in (".crease", ".git"))
        for name in sorted(files):
            path = Path(parent) / name
            result[path.relative_to(directory).as_posix()] = {
                "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                "executable": bool(path.stat().st_mode & 0o111),
            }
    return result


class Suite:
    def __init__(self, args, root, helper):
        self.args, self.root, self.helper = args, root, helper
        self.binary = str(args.binary.resolve())
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(("GIT_", "CREASE_")) and k != "SSH_AUTH_SOCK"}
        self.env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                        GIT_TERMINAL_PROMPT="0", CREASE_HOME=str(root / "identity-settings"),
                        CREASE_AUTHOR="Stress benchmark <bench@example.invalid>", LC_ALL="C")
        self.samples = {"crease": {}, "git": {}}
        self.lock = threading.Lock()
        self.commands = 0
        self.server = None
        self.log = None
        self.result = {
            "schema_version": 1, "timestamp_utc": datetime.now(timezone.utc).isoformat(),
            "platform": platform.platform(), "machine": platform.machine(),
            "parameters": {k: getattr(args, k) for k in ("files", "commits", "prs", "workers", "runs", "seed")},
            "scope": {
                "transport": "both SSH over IPv4 loopback; fresh connection per CLI command",
                "git_host": "benchmark-only Go SSH adapter running stock git-upload-pack/git-receive-pack",
                "caches": "warm OS caches; no filesystem monitors; process startup included",
                "durability": "Crease defaults including fsync/SQLite FULL; Git defaults",
                "clone": "Crease lazy historical blobs; Git full history, plus separate depth-1 sample",
                "reviews": "Crease only; Git has no pull-request service; Git branch/merge workflow is separate",
                "concurrency": "independent workspaces with one shared identity; no WAN or multi-host load",
                "storage": "logical and allocated bytes; Git automatic maintenance disabled during timings; explicit gc reported separately",
            },
            "checkpoints": [], "storage": {}, "checks": [],
        }

    def record(self, tool, name, milliseconds):
        with self.lock:
            self.samples[tool].setdefault(name, []).append(milliseconds)

    def run(self, cwd, command, *, tool=None, metric=None, expected=0):
        started = time.perf_counter()
        proc = subprocess.run([str(x) for x in command], cwd=cwd, env=self.env,
                              capture_output=True, text=True, timeout=180)
        elapsed = (time.perf_counter() - started) * 1000
        with self.lock:
            self.commands += 1
        require(proc.returncode == expected,
                f"{command!r} exited {proc.returncode}, expected {expected}:\n{proc.stdout}\n{proc.stderr}")
        if metric:
            self.record(tool, metric, elapsed)
        for line in proc.stderr.splitlines():
            if tool == "crease" and line.startswith("clone timings: "):
                with self.lock:
                    self.result.setdefault("clone_phases", []).append({"metric": metric,
                        **json.loads(line.removeprefix("clone timings: "))})
        return proc.stdout, elapsed

    def cli(self, tool, cwd, *args, metric=None, expected=0):
        prefix = ["git"] if tool == "git" else [self.binary, "--key", self.root / "identity",
                                                       "--known-hosts", self.root / "known_hosts"]
        return self.run(cwd, prefix + list(args), tool=tool, metric=metric, expected=expected)

    def configure_git(self, directory):
        for key, value in (("user.name", "Stress benchmark"), ("user.email", "bench@example.invalid"),
                           ("commit.gpgsign", "false"), ("core.hooksPath", os.devnull),
                           ("gc.auto", "0"), ("maintenance.auto", "false")):
            self.cli("git", directory, "config", key, value)

    def start(self):
        self.run(self.root, ["git", "init", "--bare", "--initial-branch=main", self.root / "git-remote.git"])
        self.configure_git(self.root / "git-remote.git")
        self.log = open(self.root / "server.log", "w+")
        self.server = subprocess.Popen([str(self.helper), str(self.root)], env=self.env,
                                       stdout=subprocess.PIPE, stderr=self.log, text=True)
        with selectors.DefaultSelector() as selector:
            selector.register(self.server.stdout, selectors.EVENT_READ)
            require(selector.select(timeout=30), "benchmark servers did not start within 30 seconds")
            ready = self.server.stdout.readline()
        if not ready:
            self.log.seek(0)
            raise RuntimeError("benchmark servers failed: " + self.log.read())
        self.urls = json.loads(ready)
        self.env["GIT_SSH_COMMAND"] = shlex.join([
            "ssh", "-F", os.devnull, "-i", str(self.root / "identity"),
            "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=" + os.devnull,
            "-o", "UserKnownHostsFile=" + str(self.root / "known_hosts"),
            "-o", "ControlMaster=no", "-o", "ConnectTimeout=10",
        ])
        for name, command in (("git", ["git", "--version"]), ("go", ["go", "version"]),
                              ("crease", [self.binary, "--version"])):
            self.result[name] = self.run(self.root, command)[0].strip()
        self.result["crease_binary_sha256"] = hashlib.sha256(Path(self.binary).read_bytes()).hexdigest()

    def stop(self, check=True):
        if self.server is not None:
            self.server.terminate()
            try:
                self.server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait(timeout=5)
                raise RuntimeError("benchmark server needed forced shutdown")
            status = self.server.returncode
            self.server.stdout.close()
            self.server = None
            if check:
                require(status == 0, "benchmark server did not shut down cleanly")
        if self.log is not None:
            self.log.close()
            self.log = None

    def source(self, tool):
        return self.root / (tool + "-source")

    def initialize(self):
        noise = random.Random(self.args.seed).randbytes(256 * 1024)
        for tool in ("crease", "git"):
            directory = self.source(tool)
            directory.mkdir()
            self.cli(tool, directory, "init", "-b", "main")
            if tool == "git":
                self.configure_git(directory)
            for i in range(self.args.files):
                path = directory / "src" / f"group-{i // 100:04d}" / f"file-{i:06d}.txt"
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("".join(f"file {i} line {line}: deterministic fixture contents\n" for line in range(32)))
            (directory / "scattered.bin").write_bytes(noise)
            (directory / ".env").write_text("SYNTHETIC_TEST_VALUE=not-a-real-secret\n")
            (directory / "run.sh").write_text("#!/bin/sh\nprintf 'fixture\\n'\n")
            _, track = self.cli(tool, directory, "add", ".", metric="initial_add")
            _, commit = self.cli(tool, directory, "commit", "-m", "Initial fixture", metric="initial_commit")
            self.record(tool, "initial_add_and_commit", track + commit)
            if tool == "crease":
                self.cli(tool, directory, "create", self.urls["crease_url"], ".")
            else:
                self.cli(tool, directory, "remote", "add", "origin", self.urls["git_url"])
            self.cli(tool, directory, "push", "-u", "origin", "main", metric="initial_push")
        self.checkpoint(0)

    def clean(self, tool, directory):
        output, _ = self.cli(tool, directory, "status", "--porcelain=v1")
        require(not output.strip(), f"{tool} workspace is dirty: {output}")

    def checkpoint(self, commits):
        point = {"commits_after_initial": commits}
        for tool in ("crease", "git"):
            directory = self.source(tool)
            self.clean(tool, directory)
            # Age timestamps beyond both tools' conservative recent-write checks.
            time.sleep(1.1)
            point[tool] = distribution([self.cli(tool, directory, "status", "--porcelain=v1")[1]
                                        for _ in range(self.args.runs)])
            for _ in range(self.args.runs):
                self.cli(tool, directory, "log", "--oneline", "-n", "20", metric="log_20")
        require(manifest(self.source("crease")) == manifest(self.source("git")), "working trees differ at checkpoint")
        self.result["checkpoints"].append(point)
        print(f"PASS: {commits} main commits; identical trees and clean status", flush=True)

    def history(self):
        for i in range(1, self.args.commits + 1):
            # Alternate execution order to reduce consistent first/second cache bias.
            for tool in (("crease", "git") if i % 2 else ("git", "crease")):
                directory = self.source(tool)
                index = (i * 17) % self.args.files
                path = directory / "src" / f"group-{index // 100:04d}" / f"file-{index:06d}.txt"
                lines = path.read_text().splitlines(keepends=True)
                lines[i % len(lines)] = f"revision {i}, deterministic changed line\n"
                path.write_text("".join(lines))
                if i % 5 == 0:
                    path = directory / "scattered.bin"
                    data = bytearray(path.read_bytes())
                    for offset in (100, len(data) // 2, len(data) - 100):
                        data[offset] = (data[offset] + i) % 256
                    path.write_bytes(data)
                if i % 30 == 0:
                    path = directory / "run.sh"
                    path.chmod(0o755 if (i // 30) % 2 else 0o644)
                new = directory / f"temporary-{i - 1:04d}.txt"
                stage = 0
                if i % 20 == 0:
                    new = directory / f"temporary-{i:04d}.txt"
                    new.write_text(f"added at revision {i}\n")
                    _, stage = self.cli(tool, directory, "add", new.name)
                elif i % 20 == 1 and new.exists():
                    new.unlink()
                status, _ = self.cli(tool, directory, "status", "--porcelain=v1", metric="dirty_status")
                require(bool(status.strip()), f"{tool} missed dirty files at revision {i}")
                diff, _ = self.cli(tool, directory, "diff", metric="working_diff")
                require(bool(diff.strip()), f"{tool} produced an empty diff at revision {i}")
                if tool == "git":
                    _, extra = self.cli(tool, directory, "add", "-u")
                    stage += extra
                _, commit = self.cli(tool, directory, "commit", "-m", f"Revision {i}", metric="commit_command")
                self.record(tool, "commit_workflow", stage + commit)
                if i % 10 == 0 or i == self.args.commits:
                    self.cli(tool, directory, "push", "origin", "main", metric="incremental_push")
            if i % 50 == 0 or i == self.args.commits:
                self.checkpoint(i)
        for tool in ("crease", "git"):
            directory = self.source(tool)
            for _ in range(self.args.runs):
                self.cli(tool, directory, "diff", "HEAD~1", "HEAD", metric="revision_diff")
            history, _ = self.cli(tool, directory, "log", "--oneline", "-n", str(self.args.commits + 2))
            require(len(history.splitlines()) == self.args.commits + 1, f"{tool} history count differs")
        self.result["checks"].append("main history counts, dirty detection, nonempty diffs, file contents and executable modes")

    def clone(self, tool, directory, metric):
        flags = ["--timings"] if tool == "crease" else []
        self.cli(tool, self.root, "clone", self.urls[tool + "_url"], directory, *flags, metric=metric)
        if tool == "git":
            self.configure_git(directory)

    def clone_history(self):
        expected = manifest(self.source("crease"))
        for i in range(self.args.runs):
            for tool in (("crease", "git") if i % 2 else ("git", "crease")):
                directory = self.root / f"{tool}-clone-{i}"
                self.clone(tool, directory, "clone_current_checkout")
                require(manifest(directory) == expected, f"{tool} clone differs from source")
                if i == 0:
                    self.result["storage"][tool + "_clone_before_history_read"] = disk_usage(directory / (".git" if tool == "git" else ".crease"))
                # Reading the whole initial patch exceeds the CLI's intentional
                # output limit for large fixtures. The first edit reads the
                # initial blob while keeping the verification output bounded.
                historical, _ = self.cli(tool, directory, "diff", f"HEAD~{self.args.commits}",
                                         f"HEAD~{self.args.commits - 1}", metric="diff_initial_revision")
                require(f"-file {17 % self.args.files} line 1: deterministic fixture contents" in historical
                        and "+revision 1, deterministic changed line" in historical,
                        f"{tool} historical diff lost the initial or first-revision contents")
        for i in range(self.args.runs):
            directory = self.root / f"git-shallow-{i}"
            self.cli("git", self.root, "clone", "--depth", "1", "--single-branch", self.urls["git_url"],
                     directory, metric="clone_depth_1")
            require(manifest(directory) == expected, "Git shallow clone differs from source")
        self.result["checks"].append("all measured SSH clones reproduce every path, byte and executable mode; sampled initial-revision contents match")
        print("PASS: SSH clones, shallow Git clones, historical reads", flush=True)

    def review_page(self, expected):
        seen, after = [], 0
        while True:
            output, _ = self.cli("crease", self.source("crease"), "pr", "list", "--limit", "100", "--after", str(after))
            page = [int(value) for value in re.findall(r"^#(\d+) ", output, re.M)]
            if not page:
                break
            require(page == sorted(page) and page[0] > after, "PR pagination cursor did not advance")
            seen.extend(page)
            after = page[-1]
        require(len(seen) == expected and len(set(seen)) == expected, "PR pagination dropped or duplicated reviews")
        point = {"open_prs": expected}
        point["list_50"] = distribution([self.cli("crease", self.source("crease"), "pr", "list", "--limit", "50")[1]
                                          for _ in range(self.args.runs)])
        self.result.setdefault("review_checkpoints", []).append(point)

    def features(self):
        self.review_page(0)
        self.clients = {tool: [self.root / f"{tool}-worker-{i}" for i in range(self.args.workers)]
                        for tool in ("crease", "git")}
        for tool, directories in self.clients.items():
            for directory in directories:
                self.clone(tool, directory, "worker_clone")
        self.reviews = {}

        def lane(tool, worker, indices):
            directory = self.clients[tool][worker]
            for i in indices:
                self.cli(tool, directory, "switch", "-c", f"review/{i:05d}", "main", metric="feature_switch")
                path = directory / "changes" / f"review-{i:05d}.txt"
                path.parent.mkdir(exist_ok=True)
                path.write_text(f"independent review {i}\n" + "Reviewed fixture change.\n" * 16)
                _, track = self.cli(tool, directory, "add", path.relative_to(directory), metric="feature_add")
                _, commit = self.cli(tool, directory, "commit", "-m", f"Feature {i}", metric="feature_commit")
                self.record(tool, "feature_commit_workflow", track + commit)
                self.cli(tool, directory, "push", "-u", "origin", f"review/{i:05d}", metric="feature_push")
                if tool == "crease":
                    output, _ = self.cli(tool, directory, "pr", "create", "--base", "main", "--title", f"Review {i}",
                                         "--body", f"Deterministic review body {i}\n" * 16, metric="pr_create")
                    match = re.search(r"^#(\d+) ", output, re.M)
                    require(match is not None, "PR create did not return an ID")
                    review = int(match.group(1))
                    with self.lock:
                        self.reviews[i] = review
                    self.cli(tool, directory, "pr", "comment", str(review), "--body", f"Reviewed feature {i}", metric="pr_comment")
                    diff, _ = self.cli(tool, directory, "pr", "diff", str(review), metric="pr_diff_first")
                    require(f"review-{i:05d}.txt" in diff, "PR diff omitted the feature file")
                    self.cli(tool, directory, "pr", "approve", str(review), metric="pr_approve")
                    show, _ = self.cli(tool, directory, "pr", "show", str(review), metric="pr_show")
                    require(f"Reviewed feature {i}" in show, "PR show lost its comment")

        for begin in range(1, self.args.prs + 1, 50):
            end = min(begin + 50, self.args.prs + 1)
            for tool in (("crease", "git") if begin % 100 == 1 else ("git", "crease")):
                started = time.perf_counter()
                with ThreadPoolExecutor(max_workers=self.args.workers) as pool:
                    jobs = [pool.submit(lane, tool, worker,
                                        [i for i in range(begin, end) if (i - 1) % self.args.workers == worker])
                            for worker in range(self.args.workers)]
                    for job in jobs:
                        job.result()
                self.record(tool, "feature_batch_wall", (time.perf_counter() - started) * 1000)
            self.review_page(end - 1)
            print(f"PASS: {end - 1} open PRs, comments, diffs and approvals; concurrent feature pushes", flush=True)
        self.result["checks"].append("concurrent feature pushes, one PR/comment/approval per feature, PR diff paths and complete cursor pagination")

    def merges(self):
        # The Git comparison has branches and merges, but no PR metadata or queue.
        directory = self.source("git")
        self.cli("git", directory, "fetch", "origin", metric="fetch_feature_branches")
        started = time.perf_counter()
        for i in range(1, self.args.prs + 1):
            _, merge = self.cli("git", directory, "merge", "--no-ff", "--no-edit", f"origin/review/{i:05d}", metric="branch_merge")
            _, push = self.cli("git", directory, "push", "origin", "main", metric="merge_push")
            self.record("git", "branch_merge_and_push", merge + push)
        self.result["git_merge_workflow_wall_ms"] = round((time.perf_counter() - started) * 1000, 3)

        def merge_lane(worker):
            directory = self.clients["crease"][worker]
            for i in range(worker + 1, self.args.prs + 1, self.args.workers):
                output, _ = self.cli("crease", directory, "pr", "merge", str(self.reviews[i]), "--wait", metric="pr_merge_wait")
                require("Merged " in output, "merge job did not complete")
        started = time.perf_counter()
        with ThreadPoolExecutor(max_workers=self.args.workers) as pool:
            jobs = [pool.submit(merge_lane, worker) for worker in range(self.args.workers)]
            for job in jobs:
                job.result()
        self.result["crease_merge_queue_wall_ms"] = round((time.perf_counter() - started) * 1000, 3)
        for tool in ("crease", "git"):
            self.cli(tool, self.source(tool), "pull", "--ff-only", "origin", "main", metric="pull_after_merges")
            self.clean(tool, self.source(tool))
        expected = manifest(self.source("git"))
        require(manifest(self.source("crease")) == expected, "merged working trees differ")
        require(sum(name.startswith("changes/review-") for name in expected) == self.args.prs, "merged files are missing")
        expected_first_parent = 1 + self.args.commits + self.args.prs
        for tool in ("crease", "git"):
            args = ["log", "--oneline", "-n", str(expected_first_parent + 1)]
            if tool == "git":
                args.append("--first-parent")
            output, _ = self.cli(tool, self.source(tool), *args)
            require(len(output.splitlines()) == expected_first_parent, f"{tool} merged history count differs")
            clone = self.root / (tool + "-final-clone")
            self.clone(tool, clone, "clone_after_merges")
            require(manifest(clone) == expected, f"{tool} final clone lost merged data")
            self.cli(tool, clone, "fsck", metric="fsck_final_clone")
        self.result["checks"].append("all merge jobs completed; merged trees and fresh SSH clones match; first-parent counts and fsck pass")
        print(f"PASS: {self.args.prs} merges, final pull, full tree equality, history counts and fsck", flush=True)

    def finish(self):
        # Stop/checkpoint the server before inspecting its persistent database.
        self.stop()
        dbfile = self.root / "crease-server/server.db"
        with sqlite3.connect(dbfile.as_uri() + "?mode=ro", uri=True) as db:
            counts = {table: db.execute(f"SELECT count(*) FROM {table}").fetchone()[0]
                      for table in ("prs", "comments", "approvals", "jobs", "refs")}
            require(all(counts[name] == self.args.prs for name in ("prs", "comments", "approvals", "jobs")),
                    "persistent review counts differ")
            require(db.execute("SELECT count(*) FROM jobs WHERE state != 'merged'").fetchone()[0] == 0,
                    "unfinished or failed jobs persisted")
            require(db.execute("SELECT count(*) FROM prs WHERE state != 'merged'").fetchone()[0] == 0,
                    "unmerged reviews persisted")
            require(db.execute("PRAGMA integrity_check").fetchone()[0] == "ok", "server SQLite integrity check failed")
            self.result["persistent_counts"] = counts
        self.result["checks"].append("graceful server shutdown, durable review/job counts, SQLite integrity")
        for tool in ("crease", "git"):
            self.result["storage"][tool + "_source"] = disk_usage(self.source(tool) / (".crease" if tool == "crease" else ".git"))
            remote = self.root / ("crease-server" if tool == "crease" else "git-remote.git")
            self.result["storage"][tool + "_server_before_gc"] = disk_usage(remote)
        self.cli("git", self.root / "git-remote.git", "gc", metric="server_gc")
        self.result["storage"]["git_server_after_gc"] = disk_usage(self.root / "git-remote.git")
        self.result["metrics"] = {tool: {name: distribution(values) for name, values in metrics.items()}
                                  for tool, metrics in self.samples.items()}
        self.result["commands"] = self.commands
        self.result["status"] = "passed"


COMPARABLE_METRICS = (
    ("initial_add_and_commit", "Initial add + commit"), ("dirty_status", "Dirty status"),
    ("working_diff", "Working diff"), ("commit_workflow", "Incremental commit workflow"),
    ("revision_diff", "Revision diff"), ("log_20", "Log, 20 entries"),
    ("initial_push", "Initial SSH push"), ("incremental_push", "Incremental SSH push"),
    ("feature_commit_workflow", "Feature add + commit"), ("feature_push", "Concurrent feature SSH push"),
)


def latency_histogram(samples, bins=10):
    """Return counts in buckets aligned to round, whole-millisecond steps."""
    if not samples:
        return []
    if bins < 1:
        raise ValueError("histogram bins must be positive")
    low, high = min(samples), max(samples)
    target_step = max(1, (high - low) / bins)
    if target_step <= 5:
        step = 1 if target_step <= 1 else 5
    else:
        magnitude = 10 ** math.floor(math.log10(target_step))
        step = next(multiplier * magnitude for multiplier in (1, 2, 5, 10)
                    if multiplier * magnitude >= target_step)
    first = math.ceil(low / step) * step
    last = math.ceil(high / step) * step
    bounds = list(range(first, last + step, step))
    counts = [0] * len(bounds)
    for sample in samples:
        counts[bisect.bisect_left(bounds, sample)] += 1
    return list(zip(bounds, counts))


def terminal_report(result, *, width=None, ascii_only=None):
    """Render hey-style latency histograms without a terminal or ANSI support."""
    if width is None:
        width = shutil.get_terminal_size(fallback=(80, 24)).columns
    width = max(24, min(width, 120))
    if ascii_only is None:
        try:
            "\u25a0".encode(sys.stdout.encoding or "ascii")
            ascii_only = False
        except UnicodeEncodeError:
            ascii_only = True
    mark = "#" if ascii_only else "\u25a0"
    metrics = result["metrics"]
    parameters = result["parameters"]
    lines = []

    def paragraph(text):
        lines.extend(textwrap.wrap(text, width=width))

    def chart(title, values):
        lines.append("")
        paragraph(title)
        for label, value in values:
            paragraph(f"  {label}: median {value['median_ms']:.3f} ms, "
                      f"p95 {value['p95_ms']:.3f} ms, n={value['count']}")
        for label, value in values:
            samples = sorted(value["samples_ms"])
            if not samples:
                continue
            lines.append("")
            paragraph(f"  {label} summary, ms:")
            paragraph(f"    Fastest: {samples[0]:.3f}  Slowest: {samples[-1]:.3f}  "
                      f"Average: {statistics.mean(samples):.3f}")
            paragraph("  Response time histogram, ms:")
            buckets = latency_histogram(samples)
            number_width = max(len(f"{bound:.0f}") for bound, _ in buckets)
            count_width = len(str(max(count for _, count in buckets)))
            prefixes = [f"    {bound:>{number_width}.0f} [{count:>{count_width}}] |"
                        for bound, count in buckets]
            bar_width = max(1, min(40, width - max(map(len, prefixes))))
            maximum = max(count for _, count in buckets)
            for prefix, (_, count) in zip(prefixes, buckets):
                size = max(1, (count * bar_width + maximum // 2) // maximum) if count else 0
                if len(prefix) + bar_width > width:
                    paragraph(prefix.rstrip(" |"))
                    prefix = "    |"
                lines.append(prefix + mark * size)
            paragraph("  Latency distribution, ms:")
            for percentile in (10, 25, 50, 75, 90, 95, 99):
                value_ms = samples[math.ceil(len(samples) * percentile / 100) - 1]
                paragraph(f"    {percentile:2d}% in {value_ms:.3f} ms")

    paragraph("Crease stress performance")
    paragraph(f"{parameters['files']} files, {parameters['commits']} main commits, "
              f"{parameters['prs']} PRs, {parameters['workers']} clients")
    paragraph("Timings are in ms; lower is faster. Histogram bars show sample counts, "
              "scaled within each histogram. Bucket upper bounds use whole-ms steps, "
              "favoring 5 and 10 ms increments.")
    lines.append("")
    paragraph("Comparable commands")
    for name, label in COMPARABLE_METRICS:
        chart(label, [(tool.capitalize(), metrics[tool][name]) for tool in ("crease", "git")])
    lines.append("")
    paragraph("SSH clones. Different retrieval policies.")
    chart("Current checkout", [
        ("Crease lazy history", metrics["crease"]["clone_current_checkout"]),
        ("Git full history", metrics["git"]["clone_current_checkout"]),
        ("Git depth 1", metrics["git"]["clone_depth_1"]),
    ])
    lines.append("")
    paragraph("Crease reviews. Git has no built-in PR service.")
    chart("Review lifecycle", [(label, metrics["crease"][name]) for name, label in (
        ("pr_create", "Create"), ("pr_comment", "Comment"), ("pr_diff_first", "Diff"),
        ("pr_approve", "Approve"), ("pr_show", "Show"), ("pr_merge_wait", "Merge"),
    )])
    paragraph("Merge includes queueing and the CLI's 100 ms polling interval.")
    lines.append("")
    paragraph(f"Status: {result['status']}; {result['commands']} successful commands; "
              f"{result['elapsed_seconds']:.3f} s total")
    return "\n".join(lines)


def report(result):
    metrics = result["metrics"]
    parameters = result["parameters"]
    lines = ["# Long-history and review stress test", "", f"Run: {result['timestamp_utc']}", "",
             f"{result['platform']}, {result['git']}, {result['go']}.", "",
             f"{parameters['files']} generated text files, a 256 KiB random binary, a synthetic `.env`, and an executable-mode fixture. "
             f"{parameters['commits']} main commits, {parameters['prs']} feature commits and {parameters['prs']} merge commits per tool. "
             f"{parameters['workers']} independent clients publish features concurrently.", "",
             "Both servers use authenticated SSH on loopback. Timings include fresh CLI processes, SSH setup where applicable, "
             "and default persistence. Filesystem monitors are off and OS caches are warm. Global Git configuration, hooks and signing "
             "are disabled. Automatic Git maintenance is deferred until a separately timed `git gc`. "
             "This is one synthetic workload on one machine; it does not measure WAN capacity, CPU consumption, RSS or wire bytes.", "",
             "## Comparable command timings", "", "Values are median / p95 milliseconds. Commit workflows include Git staging.", "",
             "| Operation | Crease | Git |", "| --- | ---: | ---: |"]
    for name, label in COMPARABLE_METRICS:
        values = [metrics[tool][name] for tool in ("crease", "git")]
        lines.append(f"| {label} | " + " | ".join(f"{v['median_ms']:.3f} / {v['p95_ms']:.3f}" for v in values) + " |")
    lines += ["", "## Clean status as history grows", "", "Median milliseconds after aging timestamps 1.1 seconds.", "",
              "| Main commits after initial | Crease | Git |", "| ---: | ---: | ---: |"]
    for point in result["checkpoints"]:
        lines.append(f"| {point['commits_after_initial']} | {point['crease']['median_ms']:.3f} | {point['git']['median_ms']:.3f} |")
    lines += ["", "## Clone scope", "",
              "Crease downloads current files and history metadata, deferring historical blobs. Git's normal clone downloads full "
              "reachable history. Its depth-1 clone is a separate current-checkout comparison and omits history metadata. "
              "All checkout contents and executable modes are verified. These are different retrieval policies.", "",
              "| SSH clone | Median ms | p95 ms |", "| --- | ---: | ---: |"]
    for tool, name, label in (("crease", "clone_current_checkout", "Crease lazy-history clone"),
                              ("git", "clone_current_checkout", "Git full-history clone"),
                              ("git", "clone_depth_1", "Git depth-1 clone")):
        value = metrics[tool][name]
        lines.append(f"| {label} | {value['median_ms']:.3f} | {value['p95_ms']:.3f} |")
    if result.get("clone_phases"):
        phases = [p for p in result["clone_phases"] if p["metric"] == "clone_current_checkout"]
        lines += ["", "## Crease clone phases", "",
                  "Median wall times from `--timings`. File reception overlaps checkout workers, so phase totals are not additive.", "",
                  "| Phase | Median ms |", "| --- | ---: |"]
        for name in ("authentication_ms", "initialize_ms", "metadata_ms", "prepare_ms", "files_ms",
                     "checkout_wait_ms", "file_sync_ms", "checkout_pipeline_ms", "publish_ms", "cleanup_ms", "total_ms"):
            lines.append(f"| {name} | {statistics.median(p[name] for p in phases):.3f} |")
        lines += ["", f"Checkout uses at most {max(p['workers'] for p in phases)} workers. "
                  f"Peak queued/active checkout payloads in these samples were {max(p['peak_checkout_bytes'] for p in phases):,} bytes. "
                  "This excludes object-store buffers, graph metadata and other process memory. "
                  f"Each combined clone used {max(p['requests'] for p in phases)} application request after capability discovery."]
    lines += ["", "## Crease review lifecycle", "", "Git has no built-in PR service, so these rows have no Git equivalent.", "",
              "| Operation | Samples | Median ms | p95 ms |", "| --- | ---: | ---: | ---: |"]
    for name in ("pr_create", "pr_comment", "pr_diff_first", "pr_approve", "pr_show", "pr_merge_wait"):
        value = metrics["crease"][name]
        lines.append(f"| {name} | {value['count']} | {value['median_ms']:.3f} | {value['p95_ms']:.3f} |")
    lines += ["", "PR merge latency includes queueing and the CLI's 100 ms polling interval. "
              f"All Crease merges completed in {result['crease_merge_queue_wall_ms'] / 1000:.3f} seconds with "
              f"{parameters['workers']} submitting clients and one server merge worker. "
              f"Git's separate sequential local merge-and-SSH-push workflow took {result['git_merge_workflow_wall_ms'] / 1000:.3f} seconds. "
              "The two workflows perform different work and use different concurrency.", "",
              "| Open PRs | List 50 median ms | p95 ms |", "| ---: | ---: | ---: |"]
    for point in result["review_checkpoints"]:
        value = point["list_50"]
        lines.append(f"| {point['open_prs']} | {value['median_ms']:.3f} | {value['p95_ms']:.3f} |")
    lines += ["", "## Storage", "", "Logical bytes and allocated filesystem blocks are both recorded in JSON. "
              "The Crease server includes reviews and its database; the Git server has no review data. "
              "Crease has no compaction or garbage collection. Git's compacted size is reported separately.", "",
              "| Repository | Logical MiB | Files | Packs |", "| --- | ---: | ---: | ---: |"]
    for name, value in result["storage"].items():
        lines.append(f"| {name} | {value['logical_bytes'] / 1048576:.3f} | {value['files']} | {value['packs']} |")
    lines += ["", "## Verification", ""]
    lines += ["- " + check for check in result["checks"]]
    lines += ["", f"Status: **{result['status']}**, {result['commands']} successful CLI/setup commands. "
              "Raw samples, versions, fixture seed, binary hash and persistent counts are in the adjacent JSON file.", ""]
    return "\n".join(lines)


def main():
    project = Path(__file__).resolve().parent.parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=project / "bin/crease")
    parser.add_argument("--files", type=int, default=1000)
    parser.add_argument("--commits", type=int, default=200)
    parser.add_argument("--prs", type=int, default=200)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--runs", type=int, default=7)
    parser.add_argument("--seed", type=int, default=417)
    parser.add_argument("--output", type=Path, default=project / "docs/benchmarks/stress-200.json")
    parser.add_argument("--report", type=Path, metavar="JSON",
                        help="print terminal charts from an existing stress result without running benchmarks")
    args = parser.parse_args()
    if args.report is not None:
        try:
            result = json.loads(args.report.read_text())
            output = terminal_report(result)
        except (OSError, ValueError, KeyError, TypeError) as error:
            parser.error(f"cannot render stress report {args.report}: {error}")
        print(output)
        return
    if min(args.files, args.commits, args.prs, args.workers, args.runs) < 1:
        parser.error("files, commits, prs, workers and runs must be positive")
    if args.commits + args.prs + 2 > 10000:
        parser.error("commits + prs must be below 9999 for complete CLI history validation")
    if not args.binary.is_file():
        parser.error("Crease binary is missing; run make build")
    started = time.perf_counter()
    with tempfile.TemporaryDirectory(prefix="crease-stress-") as temporary:
        root = Path(temporary)
        helper = root / "benchserver"
        subprocess.run(["go", "build", "-trimpath", "-o", str(helper), "./scripts/benchserver"],
                       cwd=project, check=True, timeout=180)
        suite = Suite(args, root, helper)
        try:
            suite.start()
            suite.initialize()
            suite.history()
            suite.clone_history()
            suite.features()
            suite.merges()
            suite.finish()
        finally:
            suite.stop(check=False)
        suite.result["elapsed_seconds"] = round(time.perf_counter() - started, 3)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(suite.result, indent=2) + "\n")
        markdown = args.output.with_suffix(".md")
        markdown.write_text(report(suite.result))
        print("\n" + terminal_report(suite.result), flush=True)
        print(f"PASS: results {args.output}\nReport: {markdown}", flush=True)


if __name__ == "__main__":
    main()
