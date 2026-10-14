# ictsc-kocto
// TODO(user): Add simple overview of use/purpose

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Getting Started

### CRD開発で使うmakeターゲット

CRDやcontrollerの開発では、必要なターゲットだけを使います。

```sh
make manifests generate  # API定義からCRDとDeepCopyコードを生成
make test                # unit test
make lint                # lint
make run                 # 手元でcontrollerを起動
```

`dev-deploy`以外のbranchをpushすると、GitHub ActionsがimageとGitOps用マニフェストを生成し、Argo CDがクラスタへdeployします。Makefileにはクラスタへのdeploy操作を置いていません。実際の手順は「GitHub ActionsとArgo CDによる開発deploy」を参照してください。

DependabotとRenovateが作成したbranchのpushでは、このdeploy用workflowは実行しません。botのbranchがimageとしてpushされ`dev-deploy`が書き換わると、未レビューの依存更新が共有の開発クラスタへsyncされてしまうためです。レビュー済みの変更を`main`へmergeしたときは、これまでどおりdeployします。

### 依存関係の更新とPRの検証

Dependabotが`go.mod`とGitHub Actionsの依存を毎週確認し、更新PRを作成します（`.github/dependabot.yml`）。k8s.io系とcontroller-runtime、Ginkgo/Gomegaはgroup化しているため、まとまった1本のPRとして届きます。k8s関連はcontroller-runtime、controller-tools、envtestのKubernetesバージョンが連動するため、自動マージは使わず内容を確認してからmergeします。

Pull requestではGitHub Actionsが`make lint`、`make test`、生成物の差分チェックを実行します（`.github/workflows/ci.yaml`）。DependabotのPRも同じ検証を通ります。CIが成功すると、`workflow_run`でCI完了を待つ`.github/workflows/dependabot-automerge.yaml`がDependabotのPRをsquash mergeします（Dependabotの`pull_request`では`GITHUB_TOKEN`がread-onlyになるため、CI側ではなく`workflow_run`側でmergeします）。

自動マージの可否は`hack/dependabot-automerge-gate.sh`が`gh pr diff`の`go.mod`と`.github/workflows/`だけから判定し、次の更新は自動マージせず人がレビューします。

- `k8s.io/*`と`sigs.k8s.io/*`の更新（controller-runtime、controller-tools、envtestのKubernetesバージョンが連動するため）
- majorバージョンの更新（Go moduleとGitHub Actionsの`uses:`の両方）

repository設定ではDependabot alerts、security updates、Allow auto-mergeを有効にしています。脆弱性が検出されると更新PRが作成され、security updateはgroup化の対象外なので依存ごとに個別のPRとして届きます。

`Makefile`のツールバージョン（`KUSTOMIZE_VERSION`、`CONTROLLER_TOOLS_VERSION`、`GOLANGCI_LINT_VERSION`）とE2Eのcert-managerバージョン（`test/utils/utils.go`）はDependabotの対象外なので、手動で更新します。

### Kind専用E2Eテスト

Talosの開発クラスタとは別に、DockerとKindを使う使い捨てクラスタで実行します。

```sh
make test-e2e
# Webhookを使わない現在の構成では省略可能
CERT_MANAGER_INSTALL_SKIP=true make test-e2e
```

既定のクラスタ名は`ictsc-kocto-test-e2e`です。`KIND_CLUSTER`を変更する場合も、末尾は`-e2e`にしてください。指定したクラスタはテスト成功・失敗にかかわらず削除するため、再利用したいクラスタを指定しないでください。既存クラスタの検出は名前の完全一致で行います。

クラスタ作成は一時kubeconfigを使い、テストも`kind get kubeconfig --name "$KIND_CLUSTER"`から作った専用kubeconfigだけを使います。普段のcurrent contextや`DEV_CONTEXT`には接続しません。E2Eはローカルイメージと一時マニフェストを使用し、GHCRへのpushやGitOps用のブランチ更新、ソースのイメージ設定の書き換えは行いません。

### Proxmox上のTalos開発環境

Proxmox上のVMID100を使う単一ノード環境です。Proxmox VEは9.2.20、CPUタイプは`host`、Talosは1.14.2、Kubernetesは1.36.5、KubeVirtは1.9.0です。VMID100には4 vCPU、8 GiBメモリ、32 GiBのシステムディスクを割り当てています。Kubernetesの単一ノード上でContainerDiskのVMを試す用途です。[Talos 1.14はKubernetes 1.36をサポート](https://docs.siderolabs.com/talos/v1.14/getting-started/support-matrix)し、[KubeVirt 1.9はKubernetes 1.36向け](https://github.com/kubevirt/sig-release/blob/main/releases/k8s-support-matrix.md)です。[TalosのProxmoxガイド](https://docs.siderolabs.com/talos/v1.14/platform-specific-installations/virtualized-platforms/proxmox)に従いCPUタイプを`host`にして、ホストCPU機能をゲストへ渡します。

この節のコマンドは、環境固有の値（ProxmoxホストのIPアドレス、TalosノードのIPアドレス、Proxmox用SSH鍵のパス）を`<...>`のプレースホルダで示します。実際の値は手元の環境やリポジトリ外の手順書を参照し、コマンド例の環境変数を自分の値に置き換えてから実行してください。

#### Talosの再構築

Kubernetes 1.36系のpatchは、実行時に`https://dl.k8s.io/release/stable-1.36.txt`で確認します。今回の構成では`v1.36.5`をcontrolplane／worker両方の設定に固定しました。Talos設定とProxmox VM設定はリポジトリ外へ、権限を制限して退避してください。

```sh
export TALOS_DIR="$HOME/talos-for-ictsc-kocto-test"
export TALOSCONFIG="$TALOS_DIR/talosconfig"
export PROXMOX_HOST="<ProxmoxホストのIPアドレス>"
export PROXMOX_SSH_KEY="$HOME/.ssh/<Proxmox用のSSH鍵>"
export TALOS_NODE_IP="<TalosノードのIPアドレス>"
export BACKUP_DIR="$HOME/ictsc-talos-backups/ictsc-kocto-test-$(date +%Y%m%d%H%M%S)"

umask 077
mkdir -p -m 700 "$BACKUP_DIR"
cp "$TALOS_DIR/controlplane.yaml" "$TALOS_DIR/worker.yaml" "$TALOSCONFIG" "$BACKUP_DIR/"
ssh -i "$PROXMOX_SSH_KEY" "root@$PROXMOX_HOST" 'qm config 100' > "$BACKUP_DIR/qm-100.conf"
chmod 600 "$BACKUP_DIR"/*
```

Talosの`STATE`と`EPHEMERAL`を初期化するとクラスタ状態が消えます。VMID100を停止してCPUタイプを`host`へ変更し、VMを起動した後、controlplane設定を適用してbootstrapします。

```sh
talosctl --talosconfig "$TALOSCONFIG" reset --nodes "$TALOS_NODE_IP" \
  --system-labels-to-wipe STATE --system-labels-to-wipe EPHEMERAL \
  --graceful=false --reboot=false --wait
ssh -i "$PROXMOX_SSH_KEY" "root@$PROXMOX_HOST" 'qm set 100 --cpu host && qm start 100'

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

`dev-deploy`以外のbranchへのpushでGitHub Actionsが`linux/amd64`イメージを作り、`ghcr.io/ictsc/ictsc-kocto:<commit SHA>`へpushします。Actionsは同じsource commitからCRDを生成し、image tagとpull Secretを設定したKubernetesマニフェストを`dev-deploy`branchへcommitします。Argo CDはそのbranchを監視し、変更をクラスタへ自動syncします。GitHub ActionsからクラスタAPIへの接続は不要です。[Argo CDの自動sync](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/) / [private repositoryの設定](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/)

`dev-deploy`branchはActionsが生成するため、直接編集しません。単一の開発クラスタを共有し、最後に成功したbranch pushの内容がdeployされます。CRDはクラスタ全体で共有するため、複数branchのschemaを同時には試しません。Argo CD Applicationは`config/argocd/ictsc-kocto-dev.yaml`で定義します。

Argo CD v3.5.3を導入します。GitHub Actionsが最初のpushを処理して`dev-deploy`branchを作成した後、Argo CDをbootstrapします。

```sh
export DEV_CONTEXT=admin@ictsc-kocto-test
kubectl --context="$DEV_CONTEXT" create namespace argocd --dry-run=client -o yaml \
  | kubectl --context="$DEV_CONTEXT" apply -f -
kubectl --context="$DEV_CONTEXT" apply --server-side --force-conflicts \
  --namespace=argocd \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/v3.5.3/manifests/install.yaml
kubectl --context="$DEV_CONTEXT" wait --for=condition=Established \
  crd/applications.argoproj.io --timeout=5m
kubectl --context="$DEV_CONTEXT" rollout status deployment/argocd-server \
  --namespace=argocd --timeout=5m
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

DEV_CONTEXT="$DEV_CONTEXT" ARGOCD_REPO_SSH_KEY="$ARGOCD_REPO_KEY" \
  hack/configure-argocd-repo.sh
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
DEV_CONTEXT="$DEV_CONTEXT" DEV_IMAGE_PULL_SECRET=ghcr-pull \
  hack/create-ghcr-pull-secret.sh
unset GHCR_TOKEN

kubectl --context="$DEV_CONTEXT" apply -f config/argocd/ictsc-kocto-dev.yaml
kubectl --context="$DEV_CONTEXT" get application ictsc-kocto-dev --namespace=argocd
```

CRDの登録とサンプルの受け付けを、変更を保存せずに確認できます。

```sh
kubectl --context="$DEV_CONTEXT" wait --for=condition=Established \
  crd/problems.lab.icttoracon.net crd/problemchallenges.lab.icttoracon.net --timeout=1m
kubectl --context="$DEV_CONTEXT" api-resources --api-group=lab.icttoracon.net
kubectl --context="$DEV_CONTEXT" apply --dry-run=server --namespace=default -k config/samples/
```

サンプルCRを実際に作成する場合は`kubectl --context="$DEV_CONTEXT" apply --namespace=default -k config/samples/`を使います。現状のcontrollerはVMを作成せず、ProblemのReady条件に`ProvisionerNotImplemented`を記録します。

ローカルでcontrollerを動かす場合は、次のように接続先を明示します。CRDがまだない場合はGitHub ActionsとArgo CDによるdeployの完了を待ってください。クラスタ側のcontrollerと同時起動すると両方が同じCRを処理するため、単独での動作確認には別の開発専用クラスタを使ってください。

```sh
DEV_CONTEXT="$DEV_CONTEXT" hack/dev-with-context.sh make run
```

#### ログ確認と片付け

controllerのログは`kubectl`で確認します。サンプルのProblemとProblemChallengeを削除するときも`kubectl`を使います。`kubevirt-smoke`は個別に削除してください。KubeVirtを含む環境全体を撤去するときは、KubeVirt CRを削除してからReleaseのoperatorマニフェストを削除します。

```sh
kubectl --context="$DEV_CONTEXT" logs --follow deployment/ictsc-kocto-controller-manager \
  --namespace=ictsc-kocto-system --container=manager
kubectl --context="$DEV_CONTEXT" delete --ignore-not-found --namespace=default -k config/samples
kubectl --context="$DEV_CONTEXT" delete kubevirt kubevirt --namespace=kubevirt --wait=true
kubectl --context="$DEV_CONTEXT" delete -f \
  https://github.com/kubevirt/kubevirt/releases/download/v1.9.0/kubevirt-operator.yaml
```

Proxmoxへの接続にはProxmox用のSSH鍵（環境変数`PROXMOX_SSH_KEY`）を使います。パスワード入力が求められた場合は入力せず、接続情報を確認してください。

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
