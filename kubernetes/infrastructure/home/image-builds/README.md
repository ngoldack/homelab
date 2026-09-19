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

Implementing it is **Unit 4.2** of the remediation plan. Two constraints learned
the hard way while attempting it, worth keeping:

* **`ghcr.io/sigstore/cosign/cosign` is distroless** (`Entrypoint:
  ['/ko-app/cosign']`, no `/bin/sh`). It cannot be staged with a shell command.
  Either build a small in-repo tools image (`COPY --from=` the pinned cosign and
  trivy images, built like the other Jobs here) or use a shell-capable image.
* **Job specs are immutable.** Any change must bump the Job name suffix, the
  output tag and (for repo-context Jobs) the context SHA together — Flux prunes
  the old Job. Editing `spec.template` in place leaves the Kustomization
  NotReady with `field is immutable`.

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
