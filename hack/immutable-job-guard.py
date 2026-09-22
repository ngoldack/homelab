#!/usr/bin/env python3
"""Fail when a Kubernetes Job's pod template is edited in place.

Usage:
    hack/immutable-job-guard.py [BASE_REF]        # BASE_REF also read from env BASE_REF

WHY JOBS SPECIFICALLY: a Job's spec.template is immutable once the Job exists, so
editing it in place produces a manifest the API server rejects ("field is
immutable"). Flux's apply of that one object fails, and a failed object fails the
whole Kustomization that owns it — every unrelated manifest in that Kustomization
stops reconciling too. That is not hypothetical: kubernetes/infrastructure/home/
image-builds/*.yaml (ten build Jobs today) froze exactly this way in PR #181 and
again before #192, and #184 narrowly avoided a third instance. Nothing in CI
catches it pre-merge today, because a YAML/JSON diff looks harmless.

The remedy, and the reason the Job name carries an `-sN` suffix: rename the Job
(bump the suffix) and bump the build tag + `--opt=context=#<sha>` in the same
commit, so the whole immutable pod template travels under a new name.

Comparison is semantic (parsed YAML, key order and comments ignored), per
(namespace or "default", metadata.name). A Job that does not exist at BASE_REF
under that key — a brand new Job, or the renamed Job that is the fix — is not a
violation.

Exit codes: 0 = pass, 1 = violations, 2 = environment/usage error. A repo with
zero Jobs passes. Requires: python3 + PyYAML; git for reading BASE_REF.
"""

import os
import subprocess
import sys

import yaml

TREE = "kubernetes/"
# Vendored upstream manifests: checked in for reference, never applied by Flux,
# so an in-place edit there cannot break a Kustomization.
SKIP_PREFIX = "kubernetes/infrastructure/home/agent-sandbox/upstream/"


def die(msg):
    print(f"immutable-job-guard: {msg}", file=sys.stderr)
    sys.exit(2)


def warn(msg):
    print(f"immutable-job-guard: WARN {msg}", file=sys.stderr)


def git(args, check=True):
    try:
        proc = subprocess.run(
            ["git", *args], capture_output=True, text=True, errors="replace"
        )
    except FileNotFoundError:
        die("git not found on PATH")
    if proc.returncode != 0:
        if check:
            die(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
        return None
    return proc.stdout


def resolve_base(arg):
    """BASE_REF argument, else env BASE_REF, else origin/main, else origin/HEAD.

    An explicitly named ref must resolve to a commit. Without this check a typo
    (or a CI misconfiguration) degrades every `git show <base>:<path>` to a
    failure, which the loop below would read as "new file" for every manifest —
    reporting a clean pass while the gate is effectively switched off. A
    fail-open gate is worse than no gate, so a bad ref is exit 2.
    """
    explicit = arg or os.environ.get("BASE_REF")
    if explicit:
        if not git(["rev-parse", "--verify", "--quiet", f"{explicit}^{{commit}}"], check=False):
            die(f"base ref {explicit!r} does not resolve to a commit")
        return explicit
    for ref in ("origin/main", "origin/HEAD"):
        if git(["rev-parse", "--verify", "--quiet", f"{ref}^{{commit}}"], check=False):
            return ref
    die("no base ref: pass BASE_REF (argument or env) or fetch origin/main")


def jobs_in(text, path):
    """[(namespace, name), spec.template] for every Job doc; None if unparseable."""
    try:
        docs = list(yaml.safe_load_all(text))
    except yaml.YAMLError as exc:
        warn(f"{path}: not parseable as YAML, skipped ({str(exc).splitlines()[0]})")
        return None
    jobs = []
    for doc in docs:
        collect(doc, path, jobs)
    return jobs


def collect(doc, path, jobs):
    if not isinstance(doc, dict):
        return
    if doc.get("kind") == "List":
        for item in doc.get("items") or []:
            collect(item, path, jobs)
        return
    if doc.get("kind") != "Job":
        return
    meta = doc.get("metadata") or {}
    name = meta.get("name")
    if not name:
        warn(f"{path}: Job without metadata.name, skipped")
        return
    template = (doc.get("spec") or {}).get("template")
    jobs.append(((meta.get("namespace") or "default", name), template))


def fingerprint(template):
    return yaml.safe_dump(template, sort_keys=True)


def main():
    base = resolve_base(sys.argv[1] if len(sys.argv) > 1 else None)
    paths = [p for p in (git(["ls-files", "-z", "--", TREE]) or "").split("\0") if p]
    compared = violations = 0
    for path in paths:
        if path.startswith(SKIP_PREFIX) or not path.endswith((".yaml", ".yml")):
            continue
        try:
            with open(path, encoding="utf-8") as handle:
                head = jobs_in(handle.read(), path)
        except OSError as exc:
            warn(f"{path}: unreadable, skipped ({exc})")
            continue
        if not head:
            continue
        # Absent at BASE_REF (new file) => every Job in it is new, not modified.
        base_text = git(["show", f"{base}:{path}"], check=False)
        if base_text is None:
            continue
        base_jobs = jobs_in(base_text, f"{base}:{path}")
        if base_jobs is None:
            continue  # warned already; never guess at a comparison
        before = {key: fingerprint(tmpl) for key, tmpl in base_jobs}
        for (namespace, name), template in head:
            if (namespace, name) not in before:
                continue  # new Job, or the rename that is the remedy
            compared += 1
            if before[(namespace, name)] != fingerprint(template):
                violations += 1
                print(
                    f"FAIL  {path}: Job {namespace}/{name} pod template changed in place"
                    " — Jobs are immutable, Flux cannot apply this"
                    " (see PRs #181/#192/#184)"
                )
                print(
                    "      remedy: rename the Job (bump the -sN suffix) and bump the"
                    " build tag + --opt=context=#<sha> in the same commit"
                )
    print(
        f"immutable-job-guard: {compared} job(s) compared against {base},"
        f" {violations} violation(s)"
    )
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main())