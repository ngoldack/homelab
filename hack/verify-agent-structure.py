#!/usr/bin/env python3
"""Assert the structural invariants of the hermes-agents StatefulSets.

Usage: verify-agent-structure.py <hermes-agents dir>

Each check guards a copy-paste failure that is silent until a rollout:
  1. boot-config projects every profile (the seed initContainer `cp`s each)
  2. probes name this pod's API_SERVER_PORT, not a sibling's
  3. the pod reads its own MATRIX_<suffix>_ACCESS_TOKEN
  4. the seed script only references declared env vars, and seeds every profile
"""
import sys, yaml, pathlib, re

agents_dir = pathlib.Path(sys.argv[1])
profiles = ("orchestrator", "dave", "chad", "lindner", "marius")
# The StatefulSets read these Secret key suffixes (see the bootstrap Job's case
# block, which must agree): the mapping is not the bot's name.
suffix = {"orchestrator": None, "dave": "D", "chad": "C", "lindner": "L", "marius": "M"}
expected_items = sorted([f"{p}.yaml" for p in profiles] + [f"{p}.soul.md" for p in profiles])
problems = []

def env_map(container):
    out = {}
    for e in container.get("env") or []:
        ref = ((e.get("valueFrom") or {}).get("secretKeyRef") or {})
        out[e["name"]] = (e.get("value"), ref.get("key"))
    return out

for path in sorted(agents_dir.glob("*.yaml")):
    if path.name in ("kustomization.yaml", "configmap.yaml"):
        continue
    for doc in yaml.safe_load_all(path.read_text()):
        if not doc or doc.get("kind") != "StatefulSet":
            continue
        name = doc["metadata"]["name"]
        spec = doc["spec"]["template"]["spec"]
        containers = {c["name"]: c for c in spec["containers"]}
        inits = {c["name"]: c for c in spec.get("initContainers") or []}

        # 1. the boot-config volume must project every profile, or the seed
        #    initContainer's `cp /opt/boot/<profile>.yaml` fails on the missing one.
        vols = {v["name"]: v for v in spec.get("volumes") or []}
        items = sorted(i["key"] for i in (vols.get("boot-config", {}).get("configMap") or {}).get("items", []))
        if items != expected_items:
            missing = [k for k in expected_items if k not in items]
            problems.append(f"{name}: boot-config is missing {missing}")

        main = containers.get(name) or next(iter(containers.values()))
        env = env_map(main)
        port = str(env.get("API_SERVER_PORT", (None, None))[0] or "")
        # 2. the probes must name this pod's own API port, not a sibling's.
        for probe_name in ("readinessProbe", "livenessProbe", "startupProbe"):
            probe = main.get(probe_name) or {}
            if "tcpSocket" in probe and str(probe["tcpSocket"].get("port")) != port:
                problems.append(f"{name}: {probe_name} tcpSocket port {probe['tcpSocket'].get('port')} != API_SERVER_PORT {port}")
            cmd = " ".join(probe.get("exec", {}).get("command") or [])
            m = re.search(r"127\.0\.0\.1:(\d+)", cmd)
            if m and m.group(1) != port:
                problems.append(f"{name}: {probe_name} curls :{m.group(1)}, API_SERVER_PORT is {port}")

        # 3. the pod's MATRIX_ACCESS_TOKEN must come from its own Secret key.
        #    The env var name is the plugin's (`MATRIX_ACCESS_TOKEN`); the Secret
        #    key carries the bot's suffix, so check the *reference*, not the name.
        want_suffix = suffix.get(name)
        if want_suffix:
            want_key = f"MATRIX_{want_suffix}_ACCESS_TOKEN"
            ref_keys = [v[1] for v in env.values() if v[1]]
            if want_key not in ref_keys:
                problems.append(f"{name}: no env reads the Secret key {want_key}")
            for rk in ref_keys:
                mm = re.match(r"MATRIX_([A-Z])_ACCESS_TOKEN$", rk or "")
                if mm and mm.group(1) != want_suffix:
                    problems.append(f"{name}: env reads a sibling's token ({rk})")
            want_dev = f"MATRIX_{want_suffix}_DEVICE_ID"
            if want_dev not in ref_keys:
                problems.append(f"{name}: no env reads the Secret key {want_dev}")

        # 4. the seed script references env vars that must be declared: an
        #    undeclared `$HINDSIGHT_X_API_KEY` is an unbound variable under set -eu.
        script = "\n".join(
            " ".join(c.get("args") or []) for c in inits.values()
        )
        init_env = env_map(list(inits.values())[0]) if inits else env_map(main)
        for var in set(re.findall(r"\$\{?(HINDSIGHT_[A-Z]_API_KEY)\}?", script)):
            if var not in init_env:
                problems.append(f"{name}: seed script uses unbound ${var}")
        # A HINDSIGHT_<x>_API_KEY env must come from the Secret key of the SAME
        # name: a cross-wired reference hands one profile another profile's tenant
        # key, which reads as "the bank isolation works" while it does not.
        for env_name, (_val, ref) in init_env.items():
            if env_name.startswith("HINDSIGHT_") and ref and ref != env_name:
                problems.append(f"{name}: {env_name} is sourced from Secret key {ref}")
        for p in profiles:
            if f"seed_profile {p}" not in script:
                problems.append(f"{name}: seed script does not seed {p}")
            if f"write_env {p} " not in script:
                problems.append(f"{name}: seed script does not write the headless env for {p}")

for p in problems:
    print("FAIL  " + p)
sys.exit(1 if problems else 0)
