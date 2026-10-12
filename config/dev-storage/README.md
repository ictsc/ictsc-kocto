# 検証クラスタの永続ディスク

単一ノードのTalos検証クラスタで使う構成です。CDI v1.64.0、CSI Hostpath Driver v1.18.0にreflink対応の変更を加えたイメージ、external-snapshotter v8.6.0を使用します。KubeVirt v1.9.0は導入済みであることを前提にしています。

CSI Hostpath Driverは検証用のドライバです。データとスナップショットは`test-talos-kocto-cp1`の`/var/lib/kubelet/kocto-csi`に保存します。ノードのディスクを共有するため、他の環境へ持ち込む場合はStorageClassとノード名を変更してください。Talosのリセットやノードディスクの故障からデータを保護する構成ではありません。

StorageClassは`kocto-hostpath`で、Filesystem/RWO、Immediate bindingを使用します。同じ名前のVolumeSnapshotClassをドライバの既定にします。ドライバ用のnamespaceだけにprivilegedを設定します。上流のCSIデバッグ用NodePort/socatは導入しません。

Snapshotとチームのディスクは、XFSのreflinkで元のデータブロックを共有します。作成直後は主に管理情報の分だけ増え、元のディスクやチームが書き換えたブロックは別に保存されます。PVCに表示される容量は論理容量なので、チーム数を掛けた値が実際の使用量になるとは限りません。ゲストの起動やログ書き込みでも差分は増えます。

変更は`reflink/reflink.patch`にあります。上流ソースの版とSHA256をDockerfileで固定し、Filesystem Snapshotをtarからreflinkのディレクトリへ変更しています。保存と復元で`cp --reflink=always`を使い、非対応のファイルシステムや別デバイスへのコピーは失敗させます。全コピーへは切り替えません。途中のコピーは一時ディレクトリに置き、同期後にrenameします。削除時はファイルの参照が外れ、他のチームが使っているブロックは残ります。

この変更はFilesystemに限ります。Raw Blockのコピーは上流のままで、このStorageProfileでは使用しません。旧ドライバが作ったtar形式のSnapshotからの復元は拒否します。切り替える前に既存のChallengeとSnapshotを削除してください。現在の検証クラスタでは残っていないことを確認して切り替えています。

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

controllerのdeployは通常のGitHub ActionsとArgo CD経由で行います。`config/manager/manager.yaml`には`--storage-class=kocto-hostpath`を設定しています。

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
