# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Stub for docker_image.reference, alongside the dxf stub — see dxf.py
# for why the self-test shadows the real packages rather than depending
# on them (#507).
#
# Only the three accessors the push/tag scripts use are modelled:
# split_hostname(), ['tag'] and ['name'].


class Reference(dict):
    @staticmethod
    def parse(s):
        name, _, tag = s.rpartition(':')
        if not name:
            raise ValueError(f'no tag in reference {s!r}')
        r = Reference()
        r['name'] = name
        r['tag'] = tag
        return r

    def split_hostname(self):
        hostname, _, repo = self['name'].partition('/')
        return hostname, repo
