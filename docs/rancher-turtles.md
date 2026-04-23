# Packaging and deploying to Rancher Turtles

This document describes how to package the Waldur CAPI infrastructure provider and register it with [Rancher Turtles](https://turtles.docs.rancher.com/).

---

## How provisioned clusters connect to Rancher

CAPI provisions entirely new RKE2 clusters in OpenStack — independent of the Rancher management cluster. Once the VMs are running and RKE2 is healthy, Rancher Turtles discovers the new cluster and connects it to Rancher automatically:

1. Turtles controller (running in the Rancher cluster) reads the new cluster's kubeconfig from the CAPI-generated Secret.
2. Turtles installs the **Rancher cluster agent** (`cattle-cluster-agent`) into the new cluster.
3. The `cattle-cluster-agent` connects **outbound** to the Rancher server on port 443.
4. Rancher manages the cluster through that persistent outbound connection.

The new VMs only need outbound access to the Rancher server on port 443 — no inbound connections from Rancher to the VMs are required.

Worker nodes join the RKE2 control plane on **port 9345** (the RKE2 supervisor port) using the cluster's internal network — they do not need access to the Kubernetes API server port (6443).

---

## Provider namespace

Turtles installs the provider Deployment in the **same namespace as the `CAPIProvider` CR**. If the CR is created in `cattle-turtles-system`, the controller pod runs there — not in `cluster-api-provider-waldur-system` as defined in the base manifests. Create the Secret and ConfigMap in whichever namespace the `CAPIProvider` CR is in.

---

## Compatibility matrix

| Provider | Provider API | CAPI core | CAPI Cluster API | Rancher Turtles |
|---|---|---|---|---|
| v0.1.x | `infrastructure.cluster.waldur.com/v1beta2` | v1.10.x | `cluster.x-k8s.io/v1beta1` | v0.25.x |

### Known compatibility constraints and workarounds

**1. Turtles pre-flight rejects `contract: v1beta2`**

Turtles ≤ v0.25.x refuses to install providers that declare `contract: v1beta2` in `metadata.yaml`, even when the underlying CAPI core fully supports it. Workaround: declare `contract: v1beta1` in `metadata.yaml`. This is safe — Turtles reads `metadata.yaml` only at install time; CAPI core reads the actual contract version from the CRD labels at runtime.

**2. CAPI core v1.10.x serves only `cluster.x-k8s.io/v1beta1` Cluster objects**

The `Cluster` Kubernetes API resource is served at `cluster.x-k8s.io/v1beta1` regardless of CAPI core version. When reconciling a `v1beta1` Cluster, the CAPI core controller looks for the label `cluster.x-k8s.io/v1beta1` on the infrastructure provider CRD to resolve the correct API version. The provider CRD must declare **both** labels:

```yaml
labels:
  cluster.x-k8s.io/v1beta1: v1beta2   # required: lets CAPI core match this CRD for v1beta1 Clusters
  cluster.x-k8s.io/v1beta2: v1beta2   # marks the provider contract version in code
```

Without the `v1beta1` label the CAPI cluster controller errors with:
> `cannot find any versions matching contract "cluster.x-k8s.io/v1beta1" … as contract version label(s) are either missing or empty`

Both labels are already set in the provider CRDs via the `+kubebuilder:metadata:labels` markers. No manual action needed.

**3. CAPI core manager lacks RBAC access to provider resources**

The CAPI core controller (`capi-manager` service account) needs cluster-wide list/watch access to `WaldurCluster` and `WaldurMachine` to set OwnerRefs. CAPI uses an aggregated ClusterRole (`capi-aggregated-manager-role`) that picks up rules from any ClusterRole labelled `cluster.x-k8s.io/aggregate-to-manager: "true"`.

The provider's manager ClusterRole carries this label (added via a kustomize patch in `config/rbac/manager_role_aggregation_patch.yaml`). Without it the CAPI cluster controller errors with:
> `waldurclusters.infrastructure.cluster.waldur.com is forbidden: User "system:serviceaccount:cattle-capi-system:capi-manager" cannot list resource "waldurclusters"`

---

## Configuration resources

The controller reads all site-specific configuration from two Kubernetes resources that must exist in the provider namespace before the controller pod starts.

### `waldur-api-secret` (Secret) — required

| Key | Description |
|---|---|
| `URL` | Base URL of the Waldur API (e.g. `https://waldur.example.com/api/`) |
| `TOKEN` | Waldur API token for the service account used to place VM orders |

### `waldur-controller-config` (ConfigMap) — optional keys

| Key | Required | Description |
|---|---|---|
| `VAULT_ADDR` | No | URL of the Vault server. When absent, Vault integration is disabled and bootstrap cloud-init is passed through verbatim. |
| `VAULT_APPROLE_SECRET` | No | Name of the Kubernetes Secret (in the provider namespace) holding `role_id` and `secret_id` for AppRole auth. Defaults to `waldur-vault-approle`. Used when `VAULT_K8S_ROLE` is not set. |
| `VAULT_K8S_ROLE` | No | Opt-in: Vault Kubernetes auth role name. Requires Vault to reach the Rancher cluster's Kubernetes API server on port 6443. Mutually exclusive with AppRole auth. |
| `BASE_TEMPLATE_CONFIGMAP` | No | Name of a ConfigMap (in the same namespace) containing a `cloud-init.yaml` key with static OS-level cloud-init sections merged into every node's user-data. |

---

## Vault integration

The controller supports HashiCorp Vault for securely delivering RKE2 join tokens to nodes at boot. Two authentication methods are supported depending on network topology.

### AppRole auth (default — recommended)

AppRole auth does not require Vault to reach the Rancher cluster's Kubernetes API server. It works across network boundaries and is the default when `VAULT_K8S_ROLE` is not set.

**Step 1 — Create the controller policy:**

```bash
vault policy write waldur-capi-controller - <<EOF
path "secret/data/rke2/*" {
  capabilities = ["create", "update", "read"]
}
path "auth/approle/role/+/secret-id" {
  capabilities = ["update"]
}
path "auth/approle/role/+/secret-id/destroy" {
  capabilities = ["update"]
}
EOF
```

This policy grants the controller exactly what it needs: read/write RKE2 join tokens in KV, and generate/revoke per-VM `secret_id`s for the node provisioning AppRoles.

**Step 2 — Create the controller AppRole:**

```bash
vault write auth/approle/role/waldur-controller \
  token_policies="waldur-capi-controller" \
  token_ttl=1h \
  token_max_ttl=4h
```

**Step 3 — Retrieve the credentials:**

```bash
vault read auth/approle/role/waldur-controller/role-id
vault write -f auth/approle/role/waldur-controller/secret-id
```

Use the returned `role_id` and `secret_id` when creating the `waldur-vault-approle` Secret in the installation steps below.

### Kubernetes auth (opt-in — same-network only)

Vault's Kubernetes auth method validates the controller pod's service account JWT by calling the Rancher cluster's `TokenReview` API (port 6443). This requires Vault to reach the Rancher cluster's Kubernetes API server.

> **Note:** Rancher AIO and many production Rancher setups expose the API server only on localhost — port 6443 is not reachable from outside the VM. In these cases, use AppRole auth instead.

Enable Kubernetes auth by setting `VAULT_K8S_ROLE` in the ConfigMap (see installation steps below). `VAULT_K8S_ROLE` and `VAULT_APPROLE_SECRET` are mutually exclusive — the controller exits with an error if both are set. See `docs/vault-integration.md` for the full Vault setup for Kubernetes auth.

---

## Publishing a release

### What gets published

| Artifact | Source |
|---|---|
| `dist/install.yaml` | Generated by CI via `make build-installer IMG=...` |
| `metadata.yaml` | Committed to repo root, uploaded by CI |
| Container image | Built and pushed by CI to `ghcr.io/waldur/cluster-api-provider-waldur` |

### `metadata.yaml`

Already committed to the repo root. Update `releaseSeries` when adding a new minor version:

```yaml
apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3
releaseSeries:
  - major: 0
    minor: 1
    contract: v1beta1
```

> **Note:** The provider declares both `cluster.x-k8s.io/v1beta1: v1beta2` and `cluster.x-k8s.io/v1beta2: v1beta2` labels on its CRDs. The v1beta1 label is required for CAPI core installations that only serve `cluster.x-k8s.io/v1beta1` (older Turtles/CAPI operator bundles); without it the CAPI cluster controller cannot match the infrastructure CRD and refuses to set the OwnerRef. The `contract: v1beta1` in `metadata.yaml` is a separate Turtles pre-flight workaround. Both can be removed once Turtles ships with CAPI core ≥ v1.9.0 (which serves `cluster.x-k8s.io/v1beta2`).

### Creating a release

Push a tag — CI handles everything automatically:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `.github/workflows/release.yml` workflow will:
1. Build and push the image to `ghcr.io/waldur/cluster-api-provider-waldur:v0.1.0`
2. Run `make build-installer` to generate `dist/install.yaml` with the correct image tag
3. Create a GitHub release with `dist/install.yaml` and `metadata.yaml` as assets

---

## Installation guide

Create all resources **before** applying the `CAPIProvider` CR so the pod starts cleanly. Replace `<namespace>` with the namespace where the `CAPIProvider` CR will be created (e.g. `cattle-turtles-system`).

### Step 1 — Create the API credentials Secret

```bash
kubectl create secret generic waldur-api-secret \
  --namespace <namespace> \
  --from-literal=URL=https://waldur.example.com/api/ \
  --from-literal=TOKEN=<waldur-api-token>
```

### Step 2 — Create the Vault AppRole Secret (if using Vault)

Skip this step if Vault integration is not needed.

```bash
kubectl create secret generic waldur-vault-approle \
  --namespace <namespace> \
  --from-literal=role_id=<role-id> \
  --from-literal=secret_id=<secret-id>
```

See the [Vault integration](#vault-integration) section above for how to obtain the `role-id` and `secret-id`.

### Step 3 — Create the controller ConfigMap

Without Vault:
```bash
kubectl create configmap waldur-controller-config \
  --namespace <namespace>
```

With Vault (AppRole auth — default):
```bash
kubectl create configmap waldur-controller-config \
  --namespace <namespace> \
  --from-literal=VAULT_ADDR=https://vault.example.com
# Controller reads role_id/secret_id from the "waldur-vault-approle" Secret by default.
# Override the Secret name with VAULT_APPROLE_SECRET if needed.
```

With Vault (Kubernetes auth — opt-in, requires port 6443 reachable from Vault):
```bash
kubectl create configmap waldur-controller-config \
  --namespace <namespace> \
  --from-literal=VAULT_ADDR=https://vault.example.com \
  --from-literal=VAULT_K8S_ROLE=waldur-capi-controller
```

With Vault and a custom cloud-init base template:
```bash
kubectl create configmap waldur-controller-config \
  --namespace <namespace> \
  --from-literal=VAULT_ADDR=https://vault.example.com \
  --from-literal=BASE_TEMPLATE_CONFIGMAP=cloud-init-base
```

To update values after the provider is already running:
```bash
kubectl patch configmap waldur-controller-config \
  --namespace <namespace> \
  --patch '{"data": {"VAULT_ADDR": "https://new-vault.example.com"}}'
# Then restart the controller to pick up the change:
kubectl rollout restart deployment/cluster-api-provider-waldur-controller-manager \
  --namespace <namespace>
```

### Step 4 — Install the provider via Turtles

Turtles doesn't support specifying a custom provider URL directly in the `CAPIProvider` CR. Register the provider URL first via a `ClusterctlConfig` resource in the `cattle-turtles-system` namespace:

```bash
kubectl apply -f - <<EOF
apiVersion: turtles-capi.cattle.io/v1alpha1
kind: ClusterctlConfig
metadata:
  name: clusterctl-config
  namespace: cattle-turtles-system
spec:
  providers:
    - name: waldur
      url: "https://github.com/waldur/cluster-api-provider-waldur/releases/download/v0.1.0/install.yaml"
      type: InfrastructureProvider
EOF
```

Then apply the `CAPIProvider` CR:

```bash
kubectl apply -f - <<EOF
apiVersion: turtles-capi.cattle.io/v1alpha1
kind: CAPIProvider
metadata:
  name: waldur
  namespace: cattle-turtles-system
spec:
  name: waldur
  type: infrastructure
  version: v0.1.0
EOF
```

Turtles reconciles this and installs the provider. The controller pod reads the Secret and ConfigMap at startup.

### Step 5 — Image pull secrets (private registry only)

Skip this step if using the public `ghcr.io/waldur/cluster-api-provider-waldur` image.

Create the pull secret before the provider is installed:

```bash
kubectl create secret docker-registry registry-credentials \
  --namespace <namespace> \
  --docker-server=<registry> \
  --docker-username=<user> \
  --docker-password=<token>
```

Then reference it in `config/manager/manager.yaml` before building the release manifest:

```yaml
imagePullSecrets:
  - name: registry-credentials
```
