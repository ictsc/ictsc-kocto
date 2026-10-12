# 検証クラスタの永続ディスク

単一ノードのTalos検証クラスタで使う構成です。CDI v1.64.0、CSI Hostpath Driver v1.18.0、external-snapshotter v8.6.0を使用します。KubeVirt v1.9.0は導入済みであることを前提にしています。

CSI Hostpath Driverは検証用のドライバです。データとスナップショットは`test-talos-kocto-cp1`の`/var/lib/kubelet/kocto-csi`に保存します。ノードのディスクを共有するため、他の環境へ持ち込む場合はStorageClassとノード名を変更してください。Talosのリセットやノードディスクの故障からデータを保護する構成ではありません。

StorageClassは`kocto-hostpath`で、Filesystem/RWO、Immediate bindingを使用します。同じ名前のVolumeSnapshotClassをドライバの既定にします。ドライバ用のnamespaceだけにprivilegedを設定します。上流のCSIデバッグ用NodePort/socatは導入しません。

スナップショットAPI、CSIドライバ、CDIの順に導入します。

```sh
kubectl --context=admin@ictsc-kocto-test apply --server-side -k config/dev-storage/snapshotter
kubectl --context=admin@ictsc-kocto-test apply --server-side -k config/dev-storage/hostpath
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

参考: [CSI Hostpath Driver](https://github.com/kubernetes-csi/csi-driver-host-path/tree/v1.18.0)、[CDI](https://github.com/kubevirt/containerized-data-importer/tree/v1.64.0)、[KubeVirtのSnapshot API](https://kubevirt.io/user-guide/storage/snapshot_restore_api/)
