# First-party image builds — supply-chain contract

Every image this cluster builds is produced by a Job in this directory. This
file records the contract those Jobs are supposed to satisfy, because the Jobs
themselves are long and the contract is the part that must not drift. It is
referenced from the admission policy that enforces the result
(`../kyverno-policies/policies.yaml`, `verify-images-first-party`).

## The contract (what a signed first-party image must carry)

All of it against the **digest**, never the tag:

1. **Signature** — `cosign sign --key`, with the `cosign-signing-key` keypair
   (namespace `buildkit`, SOPS-encrypted at `cosign-key.sops.yaml`).
2. **Provenance attestation** — `cosign attest --type slsaprovenance` carrying
   builder id, the source repository + revision, and any upstream clone as a
   material.
3. **SBOM** — CycloneDX, attached with `cosign attach sbom` and gate-checked
   with `trivy image --severity CRITICAL,HIGH --exit-code 1`.

**Why the order matters.** The signature must be written *last*, so its
presence proves the SBOM and the attestation exist and the vulnerability gate
passed. An image that fails the gate is then left **unsigned** — and unsigned is
exactly what the admission policy reports in Audit mode. Failing the Job is the
loud signal for a human; the missing signature is the machine-readable one.

**Why `--tlog-upload=false`.** No egress to Rekor is allowed from this cluster.
The consequence is recorded where it matters: the admission policy sets
`rekor.ignoreTlog: true`, so a **missing** log entry is accepted rather than
treated as a verification failure.

## STATUS: the build Jobs do NOT yet carry these steps

This is the honest state as of this writing, and it is why the admission policy
is still in **Audit**:

* The keypair **does** exist and works — `cosign public-key` on the private key
  in `cosign-key.sops.yaml` reproduces the inlined public key byte-for-byte, and
  a `sign-blob` / `verify-blob` roundtrip succeeds offline.
* The Jobs **do not sign**. They build, push, and stop. So the public key in the
  policy currently verifies nothing, and every first-party digest appears in the
  audit report as unsigned.

Implementing it is **Unit 4.2** of the remediation plan. Constraints learned
while attempting it, worth keeping:

* **`ghcr.io/sigstore/cosign/cosign` is distroless** (`Entrypoint:
  ['/ko-app/cosign']`, no `/bin/sh`) — it cannot be staged with a shell
  command. The cheaper path, and the one to use: stage trivy exactly as
  `../image-scan/cronjob.yaml` already does (the trivy image has a shell,
  proven by the live CronJob), and fetch the cosign binary with a pinned
  checksum (`cosign-linux-amd64` v2.4.3 = `caaad125…4708`) in the same init
  container — the buildkit CNP already allows world:443/80. Do NOT build an
  in-repo "tools image": it would live unsigned in `registry.ngoldack.de`
  and become self-referential the moment `verify-images-first-party` goes
  Enforce.
* **Job specs are immutable.** Any change must bump the Job name suffix, the
  output tag and (for repo-context Jobs) the context SHA together — Flux prunes
  the old Job. Editing `spec.template` in place leaves the Kustomization
  NotReady with `field is immutable`. A name bump forces a rebuild and a
  re-pin, so land the harness + ONE pilot Job (llama-kv-broker is the
  smallest), verify the pilot live, then fan out to the other seven — do not
  mass-edit all eight blind.
* **The CNP needs a carve-out the first run will expose**: buildkit's
  allowlist gives the client pod only the registry namespace on :5000 plus
  world :80/443, but cosign/trivy in the Job pod reach
  `registry.ngoldack.de:443` post-DNAT, which evaluates against the **gateway
  pod's identity** (same reasoning as `../hermes-egress/cilium-policy.yaml`).
  Expect drops on the pilot's first run and add the explicit egress rule then;
  do not ship it presumed-working.

## The signing key

* The **private key** belongs only in the build signer, mounted `0400`.
* The **public key** is inlined in the admission policy — deliberately, so
  verification at admission never depends on a Secret read.
* **Rotation**: generate a new keypair, write it back with `sops`, replace the
  inlined public key, rebuild and re-sign every image, then re-pin. There is no
  overlap window in the policy, so old signatures stop verifying the moment the
  public key changes — the rebuild is part of the rotation, not a follow-up.

## After a build: pin the digest

Re-pinning is mandatory after every rebuild: the signature covers the digest
that was built, so a manifest pinning an older digest is verifying a different
— still valid — artifact.

## Reading the current state

```
kubectl get policyreports -A | grep verify-images-first-party
kubectl get clusterpolicy verify-images-first-party -o yaml
```
