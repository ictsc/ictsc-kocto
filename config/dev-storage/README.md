# 検証クラスタの永続ディスク

現在の検証ではProxmox上のCephと標準のCeph-CSIを使います。構成と検証手順は[CephのREADME](ceph/README.md)にあります。以下のHostpath構成は以前のreflink検証用として残しています。

単一ノードのTalos検証クラスタで使うHostpath構成です。CDI v1.64.0、CSI Hostpath Driver v1.18.0にreflink対応の変更を加えたイメージ、external-snapshotter v8.6.0を使用します。KubeVirt v1.9.0は導入済みであることを前提にしています。

CSI Hostpath Driverは検証用のドライバです。データとスナップショットは`test-talos-kocto-cp1`の`/var/lib/kubelet/kocto-csi`に保存します。ノードのディスクを共有するため、他の環境へ持ち込む場合はStorageClassとノード名を変更してください。Talosのリセットやノードディスクの故障からデータを保護する構成ではありません。

StorageClassは`kocto-hostpath`で、Filesystem/RWO、Immediate bindingを使用します。同じ名前のVolumeSnapshotClassをドライバの既定にします。ドライバ用のnamespaceだけにprivilegedを設定します。上流のCSIデバッグ用NodePort/socatは導入しません。

Snapshotとチームのディスクは、XFSのreflinkで元のデータブロックを共有します。作成直後は主に管理情報の分だけ増え、元のディスクやチームが書き換えたブロックは別に保存されます。PVCに表示される容量は論理容量なので、チーム数を掛けた値が実際の使用量になるとは限りません。ゲストの起動やログ書き込みでも差分は増えます。

差分コピーと途中のコピーの片付けは`reflink/reflink.go`、テストは`reflink/reflink_test.go`にあります。`reflink/reflink.patch`には上流からの呼び出し変更と削除エラーの扱いだけを残しています。上流ソースの版とSHA256をDockerfileで固定し、Filesystem Snapshotをtarからreflinkのディレクトリへ変更しています。保存と復元で`cp --reflink=always`を使い、非対応のファイルシステムや別デバイスへのコピーは失敗させます。全コピーへは切り替えません。途中のコピーは一時ディレクトリに置き、同期後にrenameします。削除時はファイルの参照が外れ、他のチームが使っているブロックは残ります。

追加したGoファイルは上流の`hostpath`パッケージの型と関数を使うため、Dockerビルドで上流ソースに組み込んでテストします。先頭の`//go:build ignore`は、このリポジトリのcontroller用ビルドやCRD生成から除外するための指定です。Dockerビルドでこの指定を外してからコンパイルします。編集後は次のコマンドでフォーマットとビルドを確認できます。通常の単体テストはビルド中に実行し、XFS上でのテストは後述の手順で行います。

```sh
gofmt -w config/dev-storage/reflink/reflink.go config/dev-storage/reflink/reflink_test.go
docker buildx build --platform linux/amd64 --load \
  -t kocto-reflink-csi:verify config/dev-storage/reflink
```

この変更はFilesystemに限ります。Raw Blockのコピーは上流のままで、このStorageProfileでは使用しません。旧ドライバが作ったtar形式のSnapshotからの復元は拒否します。切り替える前に既存のChallengeとSnapshotを削除してください。現在の検証クラスタでは残っていないことを確認して切り替えています。

## ストレージに求める機能

Problem/ProblemChallengeのcontrollerはKubeVirtのSnapshot/CloneとKubernetesのPVC/VolumeSnapshotを操作します。XFSのパスやreflinkの処理はcontrollerに入れていません。差分保存はCSIとストレージが実行するため、対応する標準CSIへ切り替える場合も、CRDへストレージ固有の処理を追加する必要はありません。Snapshotに対応していることと、復元先が元のブロックを共有することは別の条件です。[KubernetesのVolumeSnapshot](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)

今のHostpath改造は、既存のXFSと保存先を使って検証するための構成です。Goファイルへ分離しても、上流ドライバの改造を保守する必要は残ります。継続して使用する基盤には、標準で差分復元に対応するCSIを選ぶ方がよいと考えます。

今後の候補はProxmox上のCeph RBDとA220のONTAPです。CephはCeph-CSI、A220はTridentを通してPVCを作ります。ProxmoxのVMディスクをCephに置くだけでは、PVCごとの差分クローンにはなりません。Kubernetesからストレージへ直接接続する必要があります。[RBDのクローン](https://docs.ceph.com/en/latest/rbd/rbd-snapshot/)、[TridentのSnapshot復元](https://docs.netapp.com/us-en/trident/trident-concepts/snapshots.html)

2026-10-13にProxmoxの未割り当て容量へ64GiBのLVを新規作成し、単一OSDのCephを構築しました。MON/MGRも各1台で、poolのsize/min_sizeは1です。TalosのOS用ディスクとは別のLVですが、同じ物理ディスク上にあります。この構成では可用性を確認しません。差分クローンの確認には標準のCeph-CSI v3.18.1を使います。

## 導入と検証

CSIイメージもGitHub Actionsでビルドし、`ghcr.io/ictsc/ictsc-kocto:storage-<commit SHA>`として公開します。CIで上流と追加の単体テストを実行します。`config/dev-storage/hostpath`はworkflowがイメージを置換してレンダリングする入力で、配置するマニフェストは`dev-deploy`の`manifests/kocto-storage.yaml`です。Argo CDがCSIドライバも適用します。

最初にSnapshot API用のnamespaceを作り、CSIイメージのpull secretを`kocto-storage`に用意します。controller用と同じGHCRの資格情報を使えます。資格情報はGitに保存しません。その後、ソースをpushしてCSIドライバを配置し、CDIを導入します。

```sh
kubectl --context=admin@ictsc-kocto-test apply --server-side -k config/dev-storage/snapshotter
# kocto-storageにghcr-pullを用意し、ソースをpushしてGitOpsのsyncを待つ
kubectl --context=admin@ictsc-kocto-test apply --server-side -f https://github.com/kubevirt/containerized-data-importer/releases/download/v1.64.0/cdi-operator.yaml
kubectl --context=admin@ictsc-kocto-test wait deployment/cdi-operator -n cdi --for=condition=Available --timeout=5m
kubectl --context=admin@ictsc-kocto-test apply --server-side -f https://github.com/kubevirt/containerized-data-importer/releases/download/v1.64.0/cdi-cr.yaml
kubectl --context=admin@ictsc-kocto-test wait cdi/cdi --for=condition=Available --timeout=5m
kubectl --context=admin@ictsc-kocto-test apply --server-side -f config/dev-storage/storageprofile.yaml
```

KubeVirtの`spec.configuration.developerConfiguration.featureGates`に`Snapshot`を追加します。既存のfeature gateがある場合は残してください。この検証クラスタには既存のfeature gateがないため、次のpatchを使っています。

```sh
kubectl --context=admin@ictsc-kocto-test patch kubevirt kubevirt -n kubevirt --type=merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"featureGates":["Snapshot"]}}}}'
kubectl --context=admin@ictsc-kocto-test rollout status statefulset/csi-hostpathplugin -n kocto-storage
kubectl --context=admin@ictsc-kocto-test get storageclass,volumesnapshotclass,storageprofile
```

controllerのdeployは通常のGitHub ActionsとArgo CD経由で行います。`config/manager/manager.yaml`には`--storage-class=kocto-ceph-rbd`を設定しています。Hostpathで再検証する場合はこの設定も変更してください。

実機のXFS上で、コピー失敗時の保護、Snapshot保存、2つの復元先の独立性、CSIの再起動後の復元、Snapshot削除後のデータ保持を確認するには、CSIイメージ内のテストを実行します。FIEMAPのSHAREDフラグで、物理ブロックを共有していることも検証します。一時データはテスト終了時に削除します。

```sh
kubectl --context=admin@ictsc-kocto-test exec -n kocto-storage csi-hostpathplugin-0 -c hostpath -- \
  env KOCTO_REFLINK_TEST_DIR=/csi-data-dir /reflink-tests -test.run '^TestKoctoReflink' -test.v
```

展開したChallengeのディスクで共有量を確認する場合は、ゲストで`sync`を実行してから次のスクリプトを使います。各ファイルの割り当て量と共有量に加え、作問ディスク、Snapshot、全チームで使う物理ブロックを重複せずに数えた`unique_data_bytes`を表示します。ファイルシステムの管理情報は含みません。読み取りだけを行います。

```sh
python3 hack/inspect-reflink.py --context=admin@ictsc-kocto-test <namespace> <Challenge名>
```

参考: [CSI Hostpath Driver](https://github.com/kubernetes-csi/csi-driver-host-path/tree/v1.18.0)、[CDI](https://github.com/kubevirt/containerized-data-importer/tree/v1.64.0)、[KubeVirtのSnapshot API](https://kubevirt.io/user-guide/storage/snapshot_restore_api/)
