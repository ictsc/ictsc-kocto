#!/usr/bin/env python3
"""Read RBD parents and usage for a Challenge; never write to its volumes."""

import argparse
import json
from pathlib import Path
import shlex
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--context', required=True)
parser.add_argument('--ssh-target', required=True)
parser.add_argument('--ssh-key', type=Path, required=True)
parser.add_argument('namespace')
parser.add_argument('challenge')
args = parser.parse_args()


def get(kind, name, namespace=None):
    scope = ['-n', namespace] if namespace else []
    output = subprocess.check_output([
        'kubectl', '--context=' + args.context, *scope, 'get', kind, name,
        '-o', 'json',
    ], text=True)
    return json.loads(output)


def rbd(command, pool, image):
    # Quote Kubernetes/Ceph names before sending a command through SSH's shell.
    remote = shlex.join(['rbd', command, '--pool', pool, '--image', image,
                         '--format', 'json'])
    return json.loads(subprocess.check_output([
        'ssh', '-o', 'BatchMode=yes', '-i', str(args.ssh_key.expanduser()),
        args.ssh_target, remote,
    ], text=True))


def pvc_image(reference):
    pvc = get('pvc', reference['name'], args.namespace)
    if pvc['metadata']['uid'] != reference['uid']:
        raise SystemExit('PVC UID changed')
    pv = get('pv', pvc['spec']['volumeName'])
    if pv['spec']['csi']['driver'] != 'rbd.csi.ceph.com':
        raise SystemExit('This inspection requires standard Ceph RBD CSI')
    attributes = pv['spec']['csi']['volumeAttributes']
    if attributes.get('radosNamespace'):
        raise SystemExit('RADOS namespaces are not supported by this inspection')
    return attributes['pool'], attributes['imageName']


challenge = get('problemchallenge', args.challenge, args.namespace)
source = challenge['status']['sourceProblem']
problem = get('problem', source['name'], args.namespace)
if problem['metadata']['uid'] != source['uid']:
    raise SystemExit('Source Problem UID changed')
disk = problem['status']['diskRef']
dv = get('datavolume', disk['diskName'], args.namespace)
if dv['metadata']['uid'] != disk['uid']:
    raise SystemExit('Author DataVolume UID changed')
author_pvc = get('pvc', disk['diskName'], args.namespace)
if not any(
    owner.get('kind') == 'DataVolume'
    and owner.get('uid') == disk['uid']
    and owner.get('controller')
    for owner in author_pvc['metadata'].get('ownerReferences', [])
):
    raise SystemExit('Author PVC is not owned by the source DataVolume')
images = {'author': pvc_image({
    'name': disk['diskName'], 'uid': author_pvc['metadata']['uid'],
})}
for team in challenge['status']['teams']:
    images['team:' + team['id']] = pvc_image(team['pvcRef'])

# Include each team's ancestry to show the snapshot's shared backing volume.
# du reports the image's own objects, not bytes inherited through the parent.
result = {'volumes': {}, 'ancestors': {}}
for label, (pool, image) in images.items():
    info = rbd('info', pool, image)
    result['volumes'][label] = {
        'pool': pool, 'image': image, 'info': info, 'du': rbd('du', pool, image),
    }
    seen = set()
    while info.get('parent'):
        parent = info['parent']
        if parent.get('pool_namespace') or parent.get('trash'):
            raise SystemExit('Parent in a namespace or trash needs separate inspection')
        key = parent['pool'] + '/' + parent['image']
        if key in seen:
            raise SystemExit('Unexpected RBD parent cycle')
        seen.add(key)
        if key not in result['ancestors']:
            parent_info = rbd('info', parent['pool'], parent['image'])
            result['ancestors'][key] = {
                'info': parent_info,
                'du': rbd('du', parent['pool'], parent['image']),
            }
        info = result['ancestors'][key]['info']
print(json.dumps(result, indent=2))
