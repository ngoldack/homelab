# Democratic-CSI research — plan

slug: democratic-csi-research

## Context
Ask: research democratic-csi, compare against the official TrueNAS CSI driver.
Known dislikes about the official driver: (a) provisions **zvols** instead of datasets for NFS PVCs,
(b) object naming on the NAS is **UUID-only**.
Core question: can democratic-csi mount against **existing NFS shares** so a self-chosen
folder/dataset structure can be kept.
Deliverable: sourced research report answering R1–R3 + decision-ready adoption outline.
Read-only session (plan mode): artifacts only under `local://`, no working-tree changes.

## Already established (do not repeat)
- Repo clean at 3976b04; `kubernetes/infrastructure/home/truenas-csi/` contains
  storageclasses.yaml, helmrelease.yaml, secret.sops.yaml, namespace.yaml, kustomization.yaml.
- Preview only (needs evidence): democratic-csi splits drivers — freenas-nfs (SSH) /
  freenas-api-nfs (REST) create datasets for NFS; *-iscsi variants create zvols;
  client drivers (nfs-client, smb-client) exist and are the likely "existing share" path.

## Research questions (each answer must be sourced)
- R1 democratic-csi model: driver matrix (name → created resource → protocol → NAS API),
  step-by-step of what the NFS driver creates (dataset under datasetParentName, NFS share,
  share params), supported TrueNAS versions, release cadence, chart distribution.
- R2 existing shares & naming (core ask): (i) can nfs-client mount a pre-existing export, and
  does dynamic provisioning create per-PV subdirectories under it (exact path/name pattern)?
  (ii) can the freenas drivers target pre-existing datasets / reuse an existing share?
  (iii) exact leaf naming of created datasets/shares (pvc-<uuid>? PVC name?) and any parameter
  to customize structure; (iv) snapshot support per driver; (v) no-CSI baseline: static k8s NFS PV.
- R3 official truenas-csi: confirm/explain zvol-for-NFS behavior, object naming (UUIDs),
  support for pre-existing exports/datasets, distribution and version support, snapshot story.

## Execution order
1. Dispatch 5 parallel research subagents (one batch): DemCsiDrivers, DemCsiShares,
   OfficialTruenasCsi, RepoInventory (scout), CoexistMigrate.
   Each: web_search + URL reads, verbatim quotes with URLs, explicit "not found" markers,
   zero edits, no gates.
2. Orchestrator verification: read raw primary sources for every load-bearing claim
   (driver docs, nfs-client doc, examples dir, driver source for naming derivation,
   truenas.com CSI doc for zvol/naming). Record verified/unverified in this file as they land.
3. Reconcile conflicts; precedence: upstream repo docs/source > vendor docs > deepwiki >
   community posts. Version-pin findings. Ambiguous → both candidates marked `unverified`.
4. Write report `local://democratic-csi-research.md`: bottom-line answers → comparison table →
   concrete StorageClass YAMLs for the target model (datasets under chosen parents;
   existing-share mounting) → what switching in this repo would take (SC/consumer inventory,
   migrate mechanics) → unverified items.
5. Update this plan file with findings + recommendation; close via `xd://propose`
   (adoption plan) or `ask` if a genuine direction choice remains.

## Risks / edge cases
- Subagents lacking web access → detect and run searches directly (fallback owner: orchestrator).
- Naming ambiguity in docs → driver source is ground truth; still ambiguous → `unverified`,
  both candidates shown, never a guess as settled.
- Doc/version drift → cite version tags and dates; prefer latest release notes.
- Identity confusion (community "truenas-csi" charts vs vendor driver) → publisher verified in R3.
- User-observed cluster behavior (zvols, UUID names) = ground truth; doc contradictions reported
  as doc-lag, never used to "correct" the observation.
- Prefer English primary sources over machine-translated aggregator content.

## Verification
- Every load-bearing claim: >=1 primary source read this session; secondary corroborates only.
- Report: each claim carries URL/path; unresolved flagged `unverified`; comparison table cells sourced.
- Repo claims: exact file:line from reads/greps.

## Findings (verified 2026-09-16)
- Official truenas-csi v1.3.0 creates FILESYSTEM datasets + NFS shares for NFS PVs
  (verified in pkg/driver/controller.go createNFSVolume; zvol path only for iSCSI/NVMe-oF).
  User's zvol-for-NFS observation is inconsistent with current driver code; likely nvmeof/iSCSI
  volumes adjacent to NFS datasets. Flagged as open verification item, not "corrected" away.
- UUID naming confirmed on official driver (vendor docs + code: leaf = SanitizeVolumeName(req.Name)).
- democratic-csi v1.9.5: NFS drivers create datasets under datasetParentName; leaf default
  pvc-<uuid>, human-readable via _private.csi.volume.idTemplate (unsupported); verified in
  src/driver/index.js getVolumeIdFromCall + controller-zfs/index.js.
- Existing shares: nfs-client (subdirs under existing export, <shareBasePath>/v/<volume_id>),
  node-manual static PVs, or datasetParentName pointing at existing parent trees; no dynamic
  adoption of pre-made leaf datasets — verified in controller code.
- Full sourced report: local://democratic-csi-research.md.
- Worktree left untouched (git status clean apart from pre-existing .omp/ untracked dir).

## Assumptions (user-overridable)
- "Official truenas csi" = TrueNAS-published driver documented at
  truenas.com/docs/solutions/integrations/csidriver/ (identity re-verified in R3).
- Migration itself is not executed here; report is decision input only.
