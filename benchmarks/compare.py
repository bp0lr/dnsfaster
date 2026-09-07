"""Loopback-only comparison. Usage and interpretation are in the root README."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time

UPSTREAM = "146c9b0e24d806b25697fbb541bf9f19a3086d41"
ROOT = "example.test"


def upstream_child(config_path, mode, checkout):
    import concurrent.futures
    import dns.resolver
    import warnings

    settings = json.loads(Path(config_path).read_text())
    mapping = settings["mapping"]
    original_query = dns.resolver.Resolver.query

    def local_query(resolver, *args, **kwargs):
        original = resolver.nameservers
        resolver.port = mapping[original[0]]
        resolver.nameservers = ["127.0.0.1"]
        resolver.timeout = resolver.lifetime = 0.2
        try:
            return original_query(resolver, *args, **kwargs)
        finally:
            resolver.nameservers = original

    # Reject accidental network access outside the fixture, including HTTP.
    def loopback_only(event, args):
        if event in ("socket.connect", "socket.sendto"):
            address = args[-1]
            if address[0] != "127.0.0.1":
                raise RuntimeError("non-loopback traffic blocked")
        if event == "socket.getaddrinfo" and args[0] != "127.0.0.1":
            raise RuntimeError("external name resolution blocked")

    sys.addaudithook(loopback_only)
    dns.resolver.Resolver.query = local_query
    warnings.filterwarnings("ignore", category=DeprecationWarning)
    sys.path.insert(0, checkout)
    sys.argv = ["dnsvalidator", "-tL", settings["input"], "-r", ROOT,
                "-threads", "10", "--silent", "--no-color"]
    from dnsvalidator import dnsvalidator as upstream

    if mode == "equivalent":
        # Shared reference setup excluded in both programs for this workload.
        upstream.goodip = "192.0.2.1"
        upstream.responses = {key: {"goodip": upstream.goodip, "nxdomain": True}
                              for key in upstream.baselines}
        upstream.nxdomainchecks = [ROOT] * 5
        with concurrent.futures.ThreadPoolExecutor(max_workers=10) as pool:
            list(pool.map(upstream.resolve_address, settings["candidates"]))
    else:
        upstream.main()


class Fixture:
    def __init__(self, profiles):
        import selectors
        import socket
        import threading

        self.selector = selectors.DefaultSelector()
        self.stop = threading.Event()
        self.counts = {}
        self.mapping = {}
        self.profiles = profiles
        for key, profile in profiles.items():
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.bind(("127.0.0.1", 0))
            self.mapping[key] = sock.getsockname()[1]
            self.selector.register(sock, selectors.EVENT_READ, (key, profile))
        self.error = None
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        import dns.message
        import dns.rcode
        import dns.rrset

        try:
            while not self.stop.is_set():
                for item, _ in self.selector.select(0.02):
                    packet, peer = item.fileobj.recvfrom(4096)
                    query = dns.message.from_wire(packet)
                    key, profile = item.data
                    name = str(query.question[0].name)
                    counter = (key, name)
                    self.counts[counter] = self.counts.get(counter, 0) + 1
                    if profile == "silent":
                        continue
                    if profile == "loss" and sum(v for (k, _), v in self.counts.items() if k == key) == 1:
                        continue
                    reply = dns.message.make_response(query)
                    positive = name in (ROOT + ".", "bet365.com.", "telegram.com.")
                    if positive or profile == "wildcard":
                        address = "192.0.2.99" if profile == "wrong_a" else "192.0.2.1"
                        reply.answer.append(dns.rrset.from_text(name, 60, "IN", "A", address))
                    else:
                        reply.set_rcode(dns.rcode.NXDOMAIN)
                    item.fileobj.sendto(reply.to_wire(), peer)
        except Exception as error:
            self.error = error

    def close(self):
        self.stop.set()
        self.thread.join()
        for item in list(self.selector.get_map().values()):
            item.fileobj.close()
        self.selector.close()
        if self.error:
            raise self.error


def measured_process(command, folder):
    import psutil

    started = time.perf_counter()
    with (folder / "stdout.txt").open("w") as out, (folder / "stderr.txt").open("w") as err:
        child = subprocess.Popen(command, stdout=out, stderr=err)
        process = psutil.Process(child.pid)
        peak = 0
        while child.poll() is None:
            try:
                memory = process.memory_info()
                peak = max(peak, getattr(memory, "peak_wset", memory.rss))
            except psutil.NoSuchProcess:
                break
            if time.perf_counter() - started > 120:
                child.kill()
                child.wait()
                raise RuntimeError("benchmark exceeded 120 seconds")
            time.sleep(0.002)
        child.wait()
    elapsed = time.perf_counter() - started
    if child.returncode != 0:
        raise RuntimeError((folder / "stderr.txt").read_text())
    return elapsed, peak / (1024 * 1024), (folder / "stdout.txt").read_text().splitlines()


def run_once(args, mode, program, profiles, iteration):
    candidates = list(profiles)
    fixture = Fixture({**profiles, "1.1.1.1": "healthy", "8.8.8.8": "healthy"})
    folder = args.out / f"{mode}-{program}-{iteration}"
    folder.mkdir(parents=True, exist_ok=True)
    input_path = folder / "input.txt"
    config_path = folder / "config.json"
    input_path.write_text("\n".join(candidates) + "\n")
    config_path.write_text(json.dumps({"mapping": fixture.mapping, "input": os.path.relpath(input_path), "candidates": candidates}))
    try:
        if program == "dnsvalidator":
            command = [sys.executable, str(Path(__file__).resolve()), "--child", str(config_path), mode, str(args.upstream)]
        else:
            endpoints = [f"127.0.0.1:{fixture.mapping[key]}" for key in candidates]
            input_path.write_text("\n".join(endpoints) + "\n")
            command = [str(args.binary), "--in", str(input_path), "--domain", ROOT,
                       "--workers", "10", "--timeout", "200ms", "--qps", "1000000",
                       "--max-duration", "110s", "--precheck-tests", "0", "--out", "-", "--quiet"]
            if mode == "equivalent":
                command += ["--validation", "off", "--tests", "6", "--filter-rate", "100"]
            else:
                refs = ",".join(f"127.0.0.1:{fixture.mapping[key]}" for key in ("1.1.1.1", "8.8.8.8"))
                command += ["--baseline", refs]
        elapsed, peak, accepted = measured_process(command, folder)
    finally:
        fixture.close()
    if program == "dnsfaster":
        reverse = {f"127.0.0.1:{port}": key for key, port in fixture.mapping.items()}
        accepted = [reverse[line] for line in accepted]
    unexpected = set(accepted) - set(candidates)
    if unexpected or len(set(accepted)) != len(accepted):
        raise RuntimeError(f"unexpected or duplicate results: {accepted}")
    truth = {key for key, profile in profiles.items() if profile in ("healthy", "loss")}
    candidate_queries = sum(value for (key, _), value in fixture.counts.items() if key in profiles)
    reference_queries = sum(fixture.counts.values()) - candidate_queries
    if mode == "equivalent" and (candidate_queries != 6 * len(candidates) or reference_queries != 0):
        raise RuntimeError("equivalent workload query counts differ")
    return {"mode": mode, "program": program, "iteration": iteration,
            "seconds": elapsed, "peak_mib": peak, "candidate_queries": candidate_queries,
            "reference_queries": reference_queries, "accepted": len(accepted),
            "false_accepts": len(set(accepted) - truth), "false_rejects": len(truth - set(accepted))}


def main():
    import hashlib
    import platform
    import statistics
    import dns
    import psutil

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--upstream", type=Path, required=True)
    parser.add_argument("--out", type=Path, default=Path("dist/comparison"))
    parser.add_argument("--repeats", type=int, default=5)
    args = parser.parse_args()
    args.binary, args.upstream, args.out = args.binary.resolve(), args.upstream.resolve(), args.out.resolve()
    revision = subprocess.check_output(["git", "-C", str(args.upstream), "rev-parse", "HEAD"], text=True).strip()
    dirty = subprocess.check_output(["git", "-C", str(args.upstream), "status", "--porcelain", "--untracked-files=no"], text=True).strip()
    if revision != UPSTREAM or dirty:
        parser.error("upstream must be an unmodified checkout of " + UPSTREAM)
    if args.repeats < 1:
        parser.error("--repeats must be positive")
    rows = []
    summaries = []
    for mode in ("equivalent", "full", "correctness"):
        profiles = {f"127.1.0.{i + 1}": "healthy" for i in range(100)}
        if mode == "correctness":
            kinds = ["healthy", "wrong_a", "wildcard", "silent", "loss"]
            profiles = {f"127.1.0.{i + 1}": kinds[i % len(kinds)] for i in range(30)}
        for program in ("dnsfaster", "dnsvalidator"):
            run_once(args, mode, program, profiles, "warmup")
        for iteration in range(args.repeats):
            programs = ["dnsfaster", "dnsvalidator"]
            if iteration % 2:
                programs.reverse()
            for program in programs:
                row = run_once(args, mode, program, profiles, iteration)
                rows.append(row)
                print(json.dumps(row), flush=True)
        for program in ("dnsfaster", "dnsvalidator"):
            selected = [row for row in rows if row["mode"] == mode and row["program"] == program]
            summaries.append({"mode": mode, "program": program,
                              "median_seconds": statistics.median(row["seconds"] for row in selected),
                              "median_peak_mib": statistics.median(row["peak_mib"] for row in selected)})
    result = {"upstream_revision": UPSTREAM, "platform": platform.platform(),
              "binary_sha256": hashlib.sha256(args.binary.read_bytes()).hexdigest(),
              "go_build": subprocess.check_output(["go", "version", "-m", str(args.binary)],
                                                    env={**os.environ, "GOTOOLCHAIN": "local"}, text=True).strip(),
              "python": sys.version, "dnspython": dns.__version__, "psutil": psutil.__version__,
              "memory": "OS peak working set on Windows, sampled RSS elsewhere; fixture excluded",
              "rows": rows, "summary": summaries}
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "results.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(summaries, indent=2))


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--child":
        upstream_child(*sys.argv[2:])
    else:
        main()
