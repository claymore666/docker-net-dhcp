# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Stub for the `dxf` registry client, used by
# scripts/test-push-multiarch-plugin.sh (#507).
#
# It shadows the real package by sitting first on PYTHONPATH, which is
# what lets the self-test run in the `test` job — that job does not
# `pip install -r scripts/requirements.txt`, and a gate self-test that
# needs the release job's dependencies would be a self-test that never
# runs.
#
# Every manifest PUT is appended to $FAKE_REGISTRY_LOG as
# `<ref>\t<content-type>`, which is the whole point: the invariant under
# test is which refs get written and with which media type. A manifest
# list at the release tag is what makes a plugin uninstallable.

import hashlib
import os


def hash_bytes(b):
    return 'sha256:' + hashlib.sha256(b).hexdigest()


def _log(line):
    path = os.environ['FAKE_REGISTRY_LOG']
    with open(path, 'a', encoding='utf-8') as f:
        f.write(line + '\n')


class DXF:
    def __init__(self, host, repo, auth=None, *args, **kwargs):
        self.host = host
        self.repo = repo

    def authenticate(self, *args, **kwargs):
        return None

    def push_blob(self, filename=None, data=None, digest=None, check_exists=False):
        if digest is not None:
            _log(f'blob\t{digest}')
            return digest
        h = hashlib.sha256()
        with open(filename, 'rb') as f:
            for chunk in iter(lambda: f.read(8192), b''):
                h.update(chunk)
        d = 'sha256:' + h.hexdigest()
        _log(f'blob\t{d}')
        return d

    # common.DXF overrides set_manifest/push_manifest on top of this and
    # routes them through _request, so this is the single choke point
    # every manifest write passes through.
    def _request(self, method, path, data=None, headers=None, **kwargs):
        if method == 'put' and path.startswith('manifests/'):
            ref = path[len('manifests/'):]
            ctype = (headers or {}).get('Content-Type', '')
            _log(f'manifest\t{ref}\t{ctype}')

        # A platform whose name is in FAKE_REGISTRY_FAIL_REF fails its
        # manifest PUT, so the test can drive the partial-push guard.
        fail = os.environ.get('FAKE_REGISTRY_FAIL_REF', '')
        if fail and path.endswith(fail):
            raise RuntimeError(f'simulated registry failure for {path}')
        return None
