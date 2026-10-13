# Cephでの差分クローン検証

検証クラスタのPVCは、Proxmox上のCephへ直接接続します。TalosのVMディスクは従来のLVMに置いたままです。Ceph-CSIは上流のv3.18.1を使い、ドライバやProblem/ProblemChallengeのコードにはストレージ固有の変更を加えていません。

| 項目 | 設定 |
| --- | --- |
| Proxmox | nuta1 / 172.18.2.1 / 9.2.20 |
| Ceph | 20.2.4-pve5 / Tentacle |
| MON/MGR/OSD | 各1台 |
| OSD | idc/kocto-ceph-osd / 64GiBの新規LV |
| Pool | kocto-dev / size=1 / min_size=1 |
| Talos | test-talos-kocto-cp1 / 172.18.2.21 / v1.14.2 |
| CSI | rbd.csi.ceph.com / v3.18.1 |
| StorageClass / VolumeSnapshotClass | kocto-ceph-rbd |
| PVC | Filesystem / ext4 / RWO / Immediate |

このLVは、既存のVG idcの未割り当て容量から新規作成しました。Talos用のvm-100-disk-0/1は変更していません。同じ物理ディスクsdcを使うため、これは可用性を確かめる構成ではありません。複製数1に伴うCephのHEALTH_WARNは残ります。

## Proxmox側の構築

2026-10-13に実行した初期構築の手順です。既存構成へ繰り返し実行する手順ではありません。SSHは`ssh root@172.18.2.1 -i ~/.ssh/ictsc2026`で接続します。

```sh
apt-get install -y ceph-mon ceph-mgr ceph-osd ceph-volume
pveceph init --network 172.18.0.0/16 --size 1 --min_size 1
pveceph mon create --mon-address 172.18.2.1
# 最初のMONを作るとMGRも作成される。
lvcreate --yes -L 64G -n kocto-ceph-osd idc
install -d -o ceph -g ceph /var/lib/ceph/bootstrap-osd
ceph auth get client.bootstrap-osd -o /var/lib/ceph/bootstrap-osd/ceph.keyring
chown ceph:ceph /var/lib/ceph/bootstrap-osd/ceph.keyring
ceph-volume lvm create --bluestore --data idc/kocto-ceph-osd
pveceph pool create kocto-dev --size 1 --min_size 1 \
  --pg_num 32 --pg_autoscale_mode on --add_storages 0
rbd pool init kocto-dev
ceph auth get-or-create client.kocto-csi \
  mon 'profile rbd' osd 'profile rbd pool=kocto-dev' \
  mgr 'profile rbd pool=kocto-dev' \
  -o /etc/ceph/ceph.client.kocto-csi.keyring
ceph -s
```

CSI用のユーザーはkocto-devだけにアクセスします。管理者の鍵をKubernetesへ渡しません。Cephの設定とkeyringはProxmox側に保存し、CSI用の資格情報はKubernetesのSecretにだけ保存します。

## Kubernetes側の配置

`config.json`とStorageClassのclusterIDは`ceph fsid`に合わせます。今のMONは172.18.2.1:6789です。Ceph-CSI用のnamespaceだけにprivilegedを許可します。単一ノードで更新できるよう、provisionerは1 replica、maxSurge=0/maxUnavailable=1にしています。Talosで使わないSELinuxのディレクトリはemptyDirへ置き換え、ログの保存先をkubelet配下にします。GroupSnapshotのCRDは導入していないため、その機能は無効にしています。

初回はnamespaceとSecretを用意します。以下のPythonは鍵をメモリ内で受け渡し、ファイルや標準出力へ書きません。

```sh
kubectl --context=admin@ictsc-kocto-test apply -f config/dev-storage/ceph/namespace.yaml
python3 - <<'PY'
import json
import subprocess
from pathlib import Path
key = subprocess.check_output([
    'ssh', '-o', 'BatchMode=yes', '-i', str(Path.home() / '.ssh/ictsc2026'),
    'root@172.18.2.1', 'ceph auth get-key client.kocto-csi'
], text=True).strip()
secret = {
    'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
    'metadata': {'name': 'kocto-ceph-rbd', 'namespace': 'kocto-ceph-storage'},
    'stringData': {'userID': 'kocto-csi', 'userKey': key},
}
subprocess.run([
    'kubectl', '--context=admin@ictsc-kocto-test', 'apply', '--server-side', '-f', '-'
], input=json.dumps(secret), text=True, check=True)
PY
```

CSIの構成はGitHub Actionsがレンダリングし、dev-deployの`manifests/kocto-ceph-storage.yaml`をArgo CDが適用します。初回の基盤導入には`kubectl apply --server-side -k config/dev-storage/ceph`を使いました。既存のSnapshot controllerとCDIを利用します。StorageProfileのcloneStrategyはsnapshotに設定し、Problemのmanagerには`--storage-class=kocto-ceph-rbd`を渡します。

```sh
kubectl --context=admin@ictsc-kocto-test -n kocto-ceph-storage get pods
kubectl --context=admin@ictsc-kocto-test get storageclass,volumesnapshotclass
kubectl --context=admin@ictsc-kocto-test get storageprofile kocto-ceph-rbd
```

## 確認する点

Snapshotから復元したPVCのPVにはimageNameが記録されます。Proxmox側で`rbd info kocto-dev/<imageName> --format json`を実行し、parentが残っていることを確認します。`rbd du -p kocto-dev --merge-snapshots --format json`で、Snapshotを含めたRBDの使用量を確認します。PVCの1GiBという表示は論理容量であり、使用量とは異なります。

元のPVCへランダムデータを書き、Podを止めてSnapshotを作ります。そこから2つのPVCを復元し、両方のSHA256が元と一致することを確かめます。片方を書き換えた後、もう片方と元のSHA256が変わらず、書き込んだ側だけで使用量が増えることを確かめます。同じ確認をProblem/ProblemChallengeでも行います。削除時にはチームのPVCを先に削除し、SnapshotとCSIの内部イメージが片付くこと、作問VMが元のディスクで再開することも確認します。

RBDはSnapshotを親とするCoWクローンです。ext4の管理情報やVMの起動時の書き込みでも差分が増えます。`rbd du`はRBDオブジェクトの使用量で、BlueStoreの管理情報を含めた物理ディスク全体の使用量ではありません。Ceph-CSIはクローンの深さやSnapshot数によってflattenする場合があり、常に差分だけで済む保証ではありません。今回の浅いクローンでは親参照と使用量を実測します。

参考: [Ceph-CSIの配置](https://ceph.github.io/ceph-csi/rbd/deploy/)、[RBDのSnapshotとクローン](https://docs.ceph.com/en/latest/rbd/rbd-snapshot/)、[LVからのOSD作成](https://docs.ceph.com/en/latest/ceph-volume/lvm/create/)
