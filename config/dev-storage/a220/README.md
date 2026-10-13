# A220用の設定例

A220は未到着のため、ここにある設定はすべて実機未検証です。Trident 26.06の公式資料を元に、ontap-san/iSCSI/Blockとontap-nas/NFS/Filesystemを選べる例を用意しています。Tridentの導入、A220への接続、FlexCloneによる差分共有、ゲストの容量一致、再接続やmultipathの動作は確認していません。現在のTalosクラスタにはこれらの設定を適用していません。

| Problemの指定 | backend | CDI StorageProfile | VolumeSnapshotClass |
| --- | --- | --- | --- |
| kocto-a220-san-block / Block | a220-san / ontap-san / iSCSI | Block / RWO / snapshot | kocto-a220 |
| kocto-a220-nas / Filesystem | a220-nas / ontap-nas / NFSv4.1 | Filesystem / RWO / snapshot | kocto-a220 |

`storageclasses.yaml`、`snapshotclass.yaml`、`storageprofiles.yaml`はKustomizeでまとめてレンダリングできます。Ceph用のGitOpsの対象には入れていません。同じCSI driverの既定VolumeSnapshotClassを複数作らないでください。SAN/NASの双方で同じClassを使い、復元の接続方法はSnapshotに記録された元PVCのvolumeModeを引き継ぎます。SANのBlockクラスにはfsTypeを指定しません。[ONTAP SANの対応モード](https://docs.netapp.com/us-en/trident/trident-use/ontap-san.html)、[TridentのStorageClass](https://docs.netapp.com/us-en/trident/trident-use/create-stor-class.html)

`backends.yaml.example`にはTridentBackendConfigを2つ記載しています。iSCSIではdataLIFを指定せず、TridentがSVMのiSCSI LIFと経路を検出します。NASではNFS用のdataLIFとexportPolicyを指定します。managementLIF、SVM、exportPolicyなどのプレースホルダーを実機の値へ置き換えます。StorageClassのstoragePoolsはbackendNameに合わせてあり、他のONTAP backendへ誤って配置しないようにしています。ONTAPのバージョンとREST APIへの対応、権限、FlexCloneの利用条件は到着後に確認してください。[SANのbackend設定](https://docs.netapp.com/us-en/trident/trident-use/ontap-san-examples.html)、[NASのbackend設定](https://docs.netapp.com/us-en/trident/trident-use/ontap-nas-examples.html)

backendのdefaultsにはspaceReserve=noneとsplitOnClone=falseを指定しています。クローンを直ちに分離して全データを複製する設定を避けるためです。実際の共有ブロック量はONTAP側で測定する必要があります。NFS/Filesystemはdisk.imgの拡張やCDIの余裕によってゲスト容量が変わる可能性があり、容量一致の候補としてSAN/Blockを先に検証します。BlockでもLUNの最小容量や丸め、Snapshot復元時の容量制約は未検証です。[Snapshotからの復元](https://docs.netapp.com/us-en/trident/trident-concepts/snapshots.html)

資格情報の例は`credentials.yaml.example`にあり、実際の値は入れていません。値を入力したコピーはGit管理外に置き、SecretとしてTridentのnamespaceへ適用します。TridentBackendConfigにはSecretの名前だけを指定します。SAN/NASが別のSVMを使う場合は、各backendのSecretも分けてください。ここではCHAPを有効にする例までは含めていません。CHAPを使う場合は公式資料に従って双方向の資格情報を別途用意します。[Secretによるbackend認証](https://docs.netapp.com/us-en/trident/trident-use/backend-kubectl.html)

## Talosの準備

現在のTalos v1.14.2を想定した例です。Linuxのapt/systemctl手順をTalosで実行する方法ではありません。Image Factoryで、そのTalosリリースに合う次のSystem Extensionを含めたinstallerを作ります。`talos-schematic.yaml.example`が入力例です。既存の拡張がある場合は置き換えずに追加します。

- siderolabs/iscsi-tools: iscsiadmとiscsid
- siderolabs/multipath-tools: multipathdとmultipath
- siderolabs/trident-iscsi-tools: Tridentがホスト上で使うblockdev/lsscsiなど
- siderolabs/util-linux-tools: blkidなど

これは将来のノード更新を伴う作業です。A220を使うノードのTalos設定を確認し、利用するリリースのImage Factoryで拡張が提供されていることを確認してから、installerの更新方法を決めます。現在のCephノードをこの準備のために更新したり再起動したりしていません。[System Extension一覧](https://github.com/siderolabs/extensions)、[Trident用拡張の説明](https://github.com/siderolabs/extensions/blob/main/storage/trident-iscsi-tools/README.md)

`talos-iscsi.yaml.example`は追加する設定文書の例です。iSCSI用のscsi_transport_iscsi/libiscsi_tcp/iscsi_tcp、multipath用のdm_multipath/dm_round_robinを読み込みます。round-robin以外のpath_selectorに変える場合は対応するモジュールも必要です。Talos v1.14のEtcFileConfigで/etc/multipath.confを用意します。拡張がこのファイルを読む方式です。古いExtensionServiceConfig方式と混ぜないでください。[multipath拡張の設定](https://github.com/siderolabs/extensions/blob/main/storage/multipath-tools/README.md)

例ではfind_multipaths=noを指定し、NETAPP/LUN.*に一致するデバイスだけを対象にしています。vendor/productとWWIDは到着後に実機で確認し、OSディスクやCephのrbdデバイスが対象にならないことを確認してください。ONTAP向けのALUAやパス選択の設定はmultipath-toolsの既定デバイス定義も確認します。Nodeから全iSCSI LIFへのTCP 3260接続と複数経路、SVM上のiSCSI有効化、Tridentによるigroup/LUNマッピングを確認します。[Tridentのノード準備](https://docs.netapp.com/us-en/trident/trident-use/worker-node-prep.html)、[multipathの除外規則](https://github.com/opensvc/multipath-tools/blob/master/multipath/multipath.conf.5.in)

導入後は、Talosのextension一覧とext-iscsid/ext-multipathdのサービス/ログ、TridentのNode plugin、multipathの経路とセッションを確認します。サービス名とホストツールの見え方も使用する拡張の版で確認してください。設定例はレンダリングしただけで、サービスの起動や経路切り替えは未検証です。

NASだけを使う場合はiSCSI/multipathは不要です。例はNFSv4.1を選んでおり、NFSv3を使う場合はnfs-utils拡張とrpcbind/statdによるロックも別途確認します。export policyには対象ノードのNFS接続元アドレスを許可し、TCP 2049、KubeVirtの実行ユーザーによる読み書き権限も確認します。

## 到着後の検証

Tridentとbackendを導入して、StorageProfileのstatusに接続方法とsnapshotClassが反映されたことを確認します。ProblemにstorageClassNameとvolumeModeを明示し、Ceph/Blockで行った検証を繰り返します。ゲスト全体のディスク容量は/sys/class/block/vda/sizeの値に512を掛けてバイトへ変換します。パーティションやdfの容量とは分けて比較します。

複数チームの初期データのSHA256、片方の書き込みの独立性、ONTAP側でのSnapshot/FlexCloneの親子関係と使用量、Challenge削除後の作問ディスクと容量の保持を確認します。最初は少数のクローンを使い、複製による使用量と管理情報を区別します。LUNとNFSの接続の可用性は、この単一ノードでの容量検証とは別に確認してください。
