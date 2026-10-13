# Cephでの差分クローン検証

Filesystem用の`kocto-ceph-rbd`を残したまま、Block用の`kocto-ceph-rbd-block`を追加しています。両方で同じCeph poolとSecret、VolumeSnapshotClass `kocto-ceph-rbd`を使います。同じCSI driverに対する既定SnapshotClassを増やさず、保存元PVCのvolumeModeで区別します。Block用StorageProfileはBlock/RWO、cloneStrategyはsnapshotです。

Problemで次のように指定すると、managerの既定Filesystemクラスを使わず、BlockのDataVolume/PVCから作問VMを起動します。既存のProblem/PVCの接続方法は変えません。

```yaml
spec:
  storage:
    storageClassName: kocto-ceph-rbd-block
    volumeMode: Block
```

FilesystemはPVC内のdisk.img、BlockはPVCのブロックデバイスをKubeVirtがゲストへ接続します。どちらも保存/復元は標準のPVC/VolumeSnapshotとKubeVirt Snapshot/Clone/Restoreで行います。Ceph専用のコピー処理はcontrollerへ追加していません。BlockではFilesystem用の余裕を加えませんが、Ceph-CSIの容量丸めは残ります。1GiBのようなGiB単位の要求で、作問VMと全チームの/sys/class/block/vda/sizeを実測して一致を確認します。

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

## TalosでのBlock deviceの権限

Blockを非rootのCDI/KubeVirtから使う場合、containerdがPodのsecurityContextに合わせてdeviceの所有者を設定する必要があります。2026-10-13の最初の試行では、CDIのrunAsUser=107に対しdeviceがroot:root/0600で、/dev/cdi-block-volumeを開く処理がPermission deniedになりました。Block PVCの作成とマッピング、Filesystemのscratch PVCの作成までは成功していました。CSIやcontrollerでdeviceをchmod/chownする処理は追加していません。

Talos v1.14/containerd 2用の追加文書が`talos-nonroot-block.yaml`です。device_ownership_from_security_context=trueをCRIに設定します。これはKubernetes用のマニフェストではなく、管理用のtalosconfigでTalosへ追加する文書です。他のCRICustomizationConfigや拡張を置き換えず、この文書を追加します。Talosは変更後にCRIを自動再起動します。Ceph/OSのディスクの再作成やiSCSI拡張の追加は不要です。[Talos v1.14のCRI設定](https://github.com/siderolabs/talos/blob/v1.14.2/website/content/v1.14/reference/configuration/cri/cricustomizationconfig.md)

```sh
# 適切な管理用talosconfigを指定し、対象ノードへ設定を追加する。
talosctl --talosconfig <talosconfig> --nodes 172.18.2.21 --endpoints 172.18.2.21 \
  patch machineconfig --mode=no-reboot --patch @config/dev-storage/ceph/talos-nonroot-block.yaml
```

2026-10-13にこの文書だけをTalosへ追加しました。dry-runと実際の適用はともにノードの再起動なしで完了し、CRIのログで設定がtrueになったことを確認しました。NodeはReadyを維持しています。初回取り込みに失敗していたCDI Podを削除して再試行し、既存のProblem/DataVolumeとroot PVCの初回取り込みを続行し、取り込みの成功と作問VMの起動を確認しました。

この設定はA220のSAN/Blockでも必要になる可能性があるruntimeの前提です。Registry importではBlockの取り込みにもFilesystemのscratch PVCを使います。今回はBlock用のクラスでscratchのFilesystem PVCも作れています。別のbackendでは、そのクラスでFilesystemを使えるか、CDIConfigのscratchSpaceStorageClassを別に指定する必要があるかを確認します。

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

起動したChallengeの親参照と使用量をまとめて読む場合は、次のコマンドを使います。このスクリプトはKubernetesの参照先UIDを確認し、Cephのinfo/duを読むだけです。CSIの実装や復元処理には関与しません。

```sh
python3 hack/inspect-ceph-clone.py \
  --context admin@ictsc-kocto-test \
  --ssh-target root@172.18.2.1 --ssh-key ~/.ssh/ictsc2026 \
  <namespace> <challenge>
```

## FilesystemのPVCとVolumeSnapshotの検証結果

2026-10-13に1GiBのFilesystem PVCへ64MiBのランダムデータを書き、Podを停止してSnapshotを作りました。同じSnapshotから2つのPVCを復元し、元のデータとSHA256が一致することを確認しました。

| RBDの使用量 | 復元直後 | aで16MiBを書き換えた後 |
| --- | --- | --- |
| 元のvolumeとそのSnapshot | 92MiB | 92MiB |
| 保存用のCSI内部イメージ | 0MiB | 0MiB |
| team-aの差分 | 8MiB | 24MiB |
| team-bの差分 | 8MiB | 8MiB |
| 合計 | 108MiB | 124MiB |

保存用のCSI内部イメージは元volumeのSnapshotを親にし、team-a/bは保存用イメージのSnapshotを親にしていました。両方の親参照が残っていることを`rbd info`で確認しました。ext4のUUID変更やmount時の書き込みにより、復元先は8MiBずつ使用しています。64MiBの元データを2回コピーしている状態ではありません。

片方の先頭16MiBを書き換えると、そのRBDの使用量だけが16MiB増えました。もう片方と元のSHA256は変わりませんでした。元のPodを再作成して再度読み取っています。チームのPVC、保存したSnapshot、元のPVCの順で片付け、CSI内部イメージとRBDのtrashに残存しないことを確認しました。

## FilesystemのProblem/ProblemChallengeの検証結果

同じCeph上でProblemから作問VMを起動し、ゲストに32MiBのランダムデータと確認用の文字列を書きました。ProblemChallengeを作ると、作問VMが停止し、SnapshotReady=Trueになり、2チームのVMがRunning/Ready=Trueになりました。両チームでデータと文字列を読み取れ、SHA256が保存時と一致しました。

| RBDの使用量 | ゲスト起動後 | aで16MiBを書き換えた後 |
| --- | --- | --- |
| 作問volumeとそのSnapshot | 160MiB | 160MiB |
| 保存用のCSI内部イメージ | 0MiB | 0MiB |
| team-aの差分 | 48MiB | 72MiB |
| team-bの差分 | 48MiB | 48MiB |
| 合計 | 256MiB | 280MiB |

両チームは保存用イメージの同じSnapshotを親にしていました。その保存用イメージも作問volumeのSnapshotを親にしており、保存と展開のどちらでも全コピーになっていません。ゲストの先頭16MiBを書き換えた後、team-bのSHA256は変わりませんでした。使用量の増加は24MiBで、4MiB単位のRBDオブジェクトのコピーや、VMの通常の書き込みも含まれます。

Challengeを削除するとチームのVM/PVCとSnapshotが削除され、作問VMが再開しました。VM/DataVolume/PVC/PVのUIDとPVCの作成時刻は変わらず、初回取り込みもやり直していません。作問VMで読み直したSHA256と確認用文字列は保存時と一致しました。元のRBDにSnapshotは残らず、CSI内部イメージとtrashも削除されました。検証用のProblemとnamespaceは最後に片付け、CephとCSIの基盤は残します。

`machine.disk: 1Gi`に対して、CDIがFilesystemの管理情報用の余裕を加えて要求し、Ceph-CSIがGiB単位で切り上げるため、PVCの容量は2GiBになりました。ゲストから見たディスクも作問VMの1GiBに対し、復元したVMでは約1.77GiBでした。内容の保存と差分共有は確認できましたが、作問VMと復元先でゲストに見えるディスクサイズは一致していません。切り上げは[Ceph-CSIの容量計算](https://github.com/ceph/ceph-csi/blob/v3.18.1/internal/util/util.go#L60)によるものです。

## Blockの実機検証

2026-10-13に新しいProblemで`kocto-ceph-rbd-block`とBlockを明示しました。managerの互換用の既定値は`kocto-ceph-rbd`のFilesystemのままです。root DataVolume/PVCは指定したBlockクラスで作られ、取り込み後のPVC容量は1GiBでした。CSI VolumeSnapshotContentのsourceVolumeModeもBlockでした。保存元と全チームでStorageClass/volumeModeが同じことを確認しています。

同じCirrosイメージで作問VMを起動し、ゲストに32MiBのランダムデータと確認用文字列を書いてsyncしました。ProblemChallengeから2チームのVMを展開し、両方で保存時のSHA256と文字列が一致しました。容量はゲストの/sys/class/block/vda/sizeに512を掛け、ディスク全体をバイト単位で比較しています。dfやパーティション容量を比べた結果ではありません。

| ゲスト | ディスク容量 |
| --- | --- |
| 作問VM / 保存前 | 1,073,741,824 bytes |
| team-a / 復元後 | 1,073,741,824 bytes |
| team-b / 復元後 | 1,073,741,824 bytes |
| team-a / 16MiBの書き換え後 | 1,073,741,824 bytes |
| team-b / 他チームの書き換え後 | 1,073,741,824 bytes |
| 作問VM / Challenge削除後の再開 | 1,073,741,824 bytes |

両チームは同じCSI内部Snapshotのクローンで、その内部イメージも作問volumeのSnapshotを親にしていました。書き換え後にも親参照が残っていることを確認しました。RBDの使用量は次のとおりです。Snapshotとheadをまとめた値であり、BlueStoreの管理情報を含む物理ディスク全体の使用量ではありません。

| RBDの使用量 | ゲスト起動後 | aで16MiBを書き換えた後 |
| --- | --- | --- |
| 作問volumeとそのSnapshot | 128MiB | 128MiB |
| 保存用のCSI内部イメージ | 0MiB | 0MiB |
| team-aの差分 | 28MiB | 52MiB |
| team-bの差分 | 28MiB | 28MiB |
| 合計 | 184MiB | 208MiB |

team-aの先頭16MiBを書き換えると、RBDオブジェクトの使用量はteam-aだけで24MiB増えました。RBDは4MiB単位でオブジェクトを共有するため、変更したファイルのバイト数と差分の使用量は同じとは限りません。team-bのSHA256は保存時のままで、容量と使用量も変わりませんでした。

保存時とteam-bのSHA256は`1e67f69e5f1cf04b58c0f98940c920bb09ed5d955444fbbc7ac5fc5dd218dd47`、書き換え後のteam-aは`1a7d72638ee16853c5616e3b71a0c52713bc7ec9bb4a5e14e57b34991ebd92b4`でした。

Challenge削除後は、同じVM/DataVolume/PVC/PVのUIDとPVCの作成時刻を維持したまま作問VMが再開しました。StorageClassはkocto-ceph-rbd-block、volumeModeはBlock、PVC容量は1GiBのままでした。ゲストでも容量は1,073,741,824バイトで、保存時のSHA256と確認用文字列が一致しました。初回取り込みをやり直していません。チームのVM/PVC、KubeVirt/CSI Snapshot、RBDの内部イメージとSnapshot/trashが削除されたことも確認しました。

別の切り分けでは、rootの検証PodからBlock PVCへ64MiBのランダムデータを書き、同じSnapshotから2つのPVCを復元しました。復元直後のクローン自身のRBDオブジェクト使用量は双方0MiBで、元の64MiBを共有していました。一方の16MiBを書き換えると、そのクローンだけが16MiBを消費しました。元PVCと他方の全デバイスのSHA256は一致したままでした。この検証のPVC/Snapshotとnamespaceは削除済みです。

## 確認範囲と制約

APIのStorageClass名とmodeの入力検証、Storageの変更/追加/削除禁止、Filesystem/Blockそれぞれの初回リソース生成と同じディスクでの復旧、managerの既定値変更後の旧Problemの再利用、保存元と異なるclass/modeの復元PVCを起動しないことをテストしました。make manifests/generate/lint-fix/test/lintを実行し、Kindの使い捨てクラスタictsc-kocto-storage-e2eでE2E 2件も通りました。Kindは削除済みです。実クラスタへはGitHub ActionsとArgo CDで反映しました。TalosのCRI文書はTalos APIから追加しています。

この容量一致はCeph RBD/Block、1GiBの要求、上記イメージと現在のCDI/KubeVirtの版での実測です。任意の容量を丸めなしで使えるという結果ではありません。Filesystemの従来の結果は上に残しています。A220は未到着なので、SAN/BlockとNAS/Filesystemの容量、差分共有、再接続はどちらも未検証です。CephのHAもこの単一OSD構成では検証していません。

参考: [Ceph-CSIの配置](https://ceph.github.io/ceph-csi/rbd/deploy/)、[RBDのSnapshotとクローン](https://docs.ceph.com/en/latest/rbd/rbd-snapshot/)、[LVからのOSD作成](https://docs.ceph.com/en/latest/ceph-volume/lvm/create/)
