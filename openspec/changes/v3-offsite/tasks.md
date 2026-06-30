## 1. Provision the offsite VM

- [ ] 1.1 Create a Talos VM on TrueNAS Scale by hand (6GB RAM, control plane role); record its IP/endpoint
- [ ] 1.2 Generate + apply the Talos single-node config (cni none, proxy disabled, allowSchedulingOnControlPlanes); bootstrap

## 2. Cilium + mesh

- [ ] 2.1 `kubernetes/clusters/offsite/` Flux entrypoint (its own Flux install) with Cilium (cluster.id 3, name offsite, shared mesh CA, clustermesh-apiserver)
- [ ] 2.2 `cilium clustermesh connect` offsite ↔ home and offsite ↔ cloud; authorize the new peer on the others
- [ ] 2.3 Verify `cilium clustermesh status` healthy across all three clusters

## 3. Minimal DR workloads

- [ ] 3.1 Decide + deploy the minimal offsite footprint (e.g. a backup landing target or a few global services); keep within 6GB
- [ ] 3.2 Validate (kustomize build clusters/offsite + kubeconform + yamllint); verify a cross-cluster global Service works to/from offsite
