# ictsc-kocto
// TODO(user): Add simple overview of use/purpose

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/ictsc-kocto:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/ictsc-kocto:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### Proxmox上のTalos開発環境

`nuta1`上のVMID100を使う単一ノード環境です。Proxmox VEは9.2.20、CPUタイプは`host`、Talosは1.14.2、Kubernetesは1.36.5、KubeVirtは1.9.0です。VMID100には4 vCPU、8 GiBメモリ、32 GiBのシステムディスクを割り当てています。Kubernetesの単一ノード上でContainerDiskのVMを試す用途です。[Talos 1.14はKubernetes 1.36をサポート](https://docs.siderolabs.com/talos/v1.14/getting-started/support-matrix)し、[KubeVirt 1.9はKubernetes 1.36向け](https://github.com/kubevirt/sig-release/blob/main/releases/k8s-support-matrix.md)です。[TalosのProxmoxガイド](https://docs.siderolabs.com/talos/v1.14/platform-specific-installations/virtualized-platforms/proxmox)に従いCPUタイプを`host`にして、ホストCPU機能をゲストへ渡します。

#### Talosの再構築

Kubernetes 1.36系のpatchは、実行時に`https://dl.k8s.io/release/stable-1.36.txt`で確認します。今回の構成では`v1.36.5`をcontrolplane／worker両方の設定に固定しました。Talos設定とProxmox VM設定はリポジトリ外へ、権限を制限して退避してください。

```sh
export TALOS_DIR="$HOME/talos-for-ictsc-kocto-test"
export TALOSCONFIG="$TALOS_DIR/talosconfig"
export TALOS_NODE_IP=172.18.2.21
export BACKUP_DIR="$HOME/ictsc-talos-backups/ictsc-kocto-test-$(date +%Y%m%d%H%M%S)"

umask 077
mkdir -p -m 700 "$BACKUP_DIR"
cp "$TALOS_DIR/controlplane.yaml" "$TALOS_DIR/worker.yaml" "$TALOSCONFIG" "$BACKUP_DIR/"
ssh -i "$HOME/.ssh/ictsc2026" root@172.18.2.1 'qm config 100' > "$BACKUP_DIR/qm-100.conf"
chmod 600 "$BACKUP_DIR"/*
```

Talosの`STATE`と`EPHEMERAL`を初期化するとクラスタ状態が消えます。VMID100を停止してCPUタイプを`host`へ変更し、VMを起動した後、controlplane設定を適用してbootstrapします。

```sh
talosctl --talosconfig "$TALOSCONFIG" reset --nodes "$TALOS_NODE_IP" \
  --system-labels-to-wipe STATE --system-labels-to-wipe EPHEMERAL \
  --graceful=false --reboot=false --wait
ssh -i "$HOME/.ssh/ictsc2026" root@172.18.2.1 'qm set 100 --cpu host && qm start 100'

talosctl version --insecure --nodes "$TALOS_NODE_IP"
talosctl apply-config --insecure --nodes "$TALOS_NODE_IP" --file "$TALOS_DIR/controlplane.yaml"
talosctl --talosconfig "$TALOSCONFIG" bootstrap --nodes "$TALOS_NODE_IP"
kubectl --context=admin@ictsc-kocto-test wait --for=condition=Ready \
  node/test-talos-kocto-cp1 --timeout=5m
```

CPU仮想化フラグとKVMデバイスを確認します。

```sh
talosctl --talosconfig "$TALOSCONFIG" read --nodes "$TALOS_NODE_IP" /proc/cpuinfo | rg -m 1 'vmx|svm'
talosctl --talosconfig "$TALOSCONFIG" list --nodes "$TALOS_NODE_IP" /dev | rg '^kvm$'
```

#### KubeVirtとテストVM

KubeVirt v1.9.0を導入し、Availableになるまで待ちます。

```sh
export DEV_CONTEXT=admin@ictsc-kocto-test
export KUBEVIRT_VERSION=v1.9.0

kubectl --context="$DEV_CONTEXT" apply -f "https://github.com/kubevirt/kubevirt/releases/download/$KUBEVIRT_VERSION/kubevirt-operator.yaml"
kubectl --context="$DEV_CONTEXT" apply -f "https://github.com/kubevirt/kubevirt/releases/download/$KUBEVIRT_VERSION/kubevirt-cr.yaml"
kubectl --context="$DEV_CONTEXT" wait --for=condition=Available kubevirt/kubevirt \
  --namespace=kubevirt --timeout=10m
```

macOS arm64用の`virtctl`を取得し、以下のテストVMマニフェストを`/tmp/kubevirt-smoke.yaml`へ保存して適用します。VMはContainerDiskとPodネットワークを使います。

```sh
mkdir -p "$HOME/.local/bin"
gh release download v1.9.0 --repo kubevirt/kubevirt \
  --pattern virtctl-v1.9.0-darwin-arm64 --dir "$HOME/.local/bin"
chmod 755 "$HOME/.local/bin/virtctl-v1.9.0-darwin-arm64"
printf '%s  %s\n' \
  8527cf7601a6d1a2fa3e8014bacacc9e7dd1a338bc572c74d3734360a344d116 \
  "$HOME/.local/bin/virtctl-v1.9.0-darwin-arm64" | shasum -a 256 -c -
kubectl --context="$DEV_CONTEXT" apply -f /tmp/kubevirt-smoke.yaml
kubectl --context="$DEV_CONTEXT" wait --for=jsonpath='{.status.phase}'=Running \
  vmi/kubevirt-smoke --namespace=default --timeout=5m
"$HOME/.local/bin/virtctl-v1.9.0-darwin-arm64" console kubevirt-smoke
```

シリアルコンソールは`Ctrl+]`で切断できます。

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: kubevirt-smoke
spec:
  runStrategy: Always
  template:
    metadata:
      labels:
        kubevirt.io/domain: kubevirt-smoke
    spec:
      domain:
        devices:
          disks:
            - name: containerdisk
              disk:
                bus: virtio
          interfaces:
            - name: default
              masquerade: {}
        resources:
          requests:
            memory: 256Mi
      networks:
        - name: default
          pod: {}
      volumes:
        - name: containerdisk
          containerDisk:
            image: quay.io/kubevirt/cirros-container-disk-demo:latest
```

テスト後はVMを削除します。

```sh
kubectl --context="$DEV_CONTEXT" delete vm kubevirt-smoke
```

#### GitHub ActionsとArgo CDによる開発deploy

任意branchへのpushでGitHub Actionsが`linux/amd64`イメージを作り、`ghcr.io/ictsc/ictsc-kocto:<commit SHA>`へpushします。Actionsは同じsource commitからCRDを生成し、image tagとpull Secretを設定したKubernetesマニフェストを`dev-deploy`branchへcommitします。Argo CDはそのbranchを監視し、変更をクラスタへ自動syncします。GitHub ActionsからクラスタAPIへの接続は不要です。[Argo CDの自動sync](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/) / [private repositoryの設定](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/)

`dev-deploy`branchはActionsが生成するため、直接編集しません。単一の開発クラスタを共有し、最後に成功したbranch pushの内容がdeployされます。CRDはクラスタ全体で共有するため、複数branchのschemaを同時には試しません。Argo CD Applicationは`config/argocd/ictsc-kocto-dev.yaml`で定義します。

Argo CD v3.5.3を導入します。GitHub Actionsが最初のpushを処理して`dev-deploy`branchを作成した後、Argo CDをbootstrapします。

```sh
export DEV_CONTEXT=admin@ictsc-kocto-test
make dev-argocd-install DEV_CONTEXT="$DEV_CONTEXT"
```

private repositoryのread-only deploy keyを作成してGitHub repositoryに登録します。公開鍵はGitHubへ登録し、秘密鍵はArgo CD namespaceのSecretにだけ保存します。

```sh
export ARGOCD_REPO_KEY="$HOME/.ssh/ictsc-kocto-argocd-readonly"
ssh-keygen -t ed25519 -N '' -C 'argocd-ictsc-kocto-readonly' -f "$ARGOCD_REPO_KEY"
gh api repos/ictsc/ictsc-kocto/keys \
  --method POST \
  -f title=argocd-ictsc-kocto-readonly \
  -f key="$(cat "$ARGOCD_REPO_KEY.pub")" \
  -F read_only=true

make dev-argocd-repo DEV_CONTEXT="$DEV_CONTEXT" \
  ARGOCD_REPO_SSH_KEY="$ARGOCD_REPO_KEY"
```

Argo CDのUIはport-forwardで開けます。

```sh
kubectl --context="$DEV_CONTEXT" port-forward --namespace=argocd \
  service/argocd-server 8080:443
```

その後`https://localhost:8080`へ接続します。initial admin passwordは次で取得できます。

```sh
kubectl --context="$DEV_CONTEXT" get secret argocd-initial-admin-secret \
  --namespace=argocd -o jsonpath='{.data.password}' | base64 --decode
```

branchへのpush後はActionsの成功とArgo CDのsyncを確認します。

```sh
gh run list --branch "$(git branch --show-current)"
kubectl --context="$DEV_CONTEXT" get application ictsc-kocto-dev --namespace=argocd
kubectl --context="$DEV_CONTEXT" get deployment ictsc-kocto-controller-manager \
  --namespace=ictsc-kocto-system
```

GHCR pull Secretには`read:packages`権限のあるトークンを使います。トークンはrepositoryやworkflow artifactへ保存しません。

```sh
export GHCR_TOKEN="$(gh auth token)"
make dev-pull-secret DEV_CONTEXT="$DEV_CONTEXT" DEV_IMAGE_PULL_SECRET=ghcr-pull
unset GHCR_TOKEN

make dev-argocd-apply DEV_CONTEXT="$DEV_CONTEXT"
kubectl --context="$DEV_CONTEXT" get application ictsc-kocto-dev --namespace=argocd
```

Argo CDを使わずにローカルからdeployする場合は、GHCRへpushしたimage tagを指定して`make dev-deploy DEV_CONTEXT="$DEV_CONTEXT" DEV_IMAGE=... DEV_IMAGE_PULL_SECRET=ghcr-pull`を実行します。Argo CDのsync対象を同時に手動更新しないでください。`make dev-image DEV_IMAGE=...`はローカルからamd64 imageをbuildx build／pushします。controllerをMac上で起動する場合は`make dev-install DEV_CONTEXT="$DEV_CONTEXT"`でCRDを入れてから`make dev-run DEV_CONTEXT="$DEV_CONTEXT"`を実行します。

#### ログ確認と片付け

`make dev-logs`はコントローラーのログをfollowします。サンプルのProblemとProblemChallengeは`make dev-cleanup DEV_CONTEXT="$DEV_CONTEXT"`で削除できます。Argo CD管理下のコントローラーとCRDを手動で片付ける場合は、先にApplicationを削除してsyncを止めてから`make dev-undeploy DEV_CONTEXT="$DEV_CONTEXT"`を実行します。`kubevirt-smoke`は個別に削除してください。KubeVirtを含む環境全体を撤去するときは、KubeVirt CRを削除してからReleaseのoperatorマニフェストを削除します。

```sh
kubectl --context="$DEV_CONTEXT" delete kubevirt kubevirt --namespace=kubevirt --wait=true
kubectl --context="$DEV_CONTEXT" delete -f \
  https://github.com/kubevirt/kubevirt/releases/download/v1.9.0/kubevirt-operator.yaml
```

Proxmox接続は`$HOME/.ssh/ictsc2026`を使います。パスワード入力が求められた場合は入力せず、接続情報を確認してください。

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/ictsc-kocto:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/ictsc-kocto/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
