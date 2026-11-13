# Tested against

The plugin is run against real DHCP servers in a separate repository, the
[docker-net-dhcp lab](https://github.com/claymore666/docker-net-dhcp-lab).
Each row below is one lab cell: a DHCP server installed the way its users
install it, and a Docker host with the plugin, both virtual machines on an
isolated network. The lab runs every scenario (a first lease, a container
restart, a host reboot, the server going away and others) on five network
shapes and judges each against the server's own lease table and a packet
capture, never against what the plugin reports. How the lab works is in
[Real-server lab](testing.md#real-server-lab).

The Fritz.Box row is the exception. It is a real home router with the Docker
host on the home network, run by its own script outside the lab's cells,
with no router login, and its packet captures are not published.

The five shapes are `bridge`, `macvlan` and `ipvlan` (the plugin as the
network driver) and `bridge-ipam` and `macvlan-ipam` (the plugin as the IPAM
driver on a bridge or macvlan network). Every cell below ran all five except
the Fritz.Box, which ran `macvlan` and `macvlan-ipam` only.

## Tested cells

Each row is the newest results page that has verdicts for that cell, so the
plugin version differs between rows. The Lab column is the lab release the
evidence is attached to and, in brackets, the lab commit the run used, which
need not be the release tag. Counts are rows: one scenario on one shape is
one row.

| DHCP server | Cell | Docker host | Plugin | Lab | Date | PASS | FAIL | BLOCKED | N/A | Results |
|---|---|---|---|---|---|---|---|---|---|---|
| Kea | `kea` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-06 | 221 | 2 | 4 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| Kea on an Ubuntu host | `host-ubuntu2404` | Ubuntu 24.04, Docker 29.9.2 | v2.7.0-rc2 | v0.4.0 (67c28eb) | 2026-10-30 | 210 | 8 | 4 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc2.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc2-evidence.tar.gz) |
| Kea on a Debian 11 host | `host-debian11-docker2010` | Debian 11, Docker 20.10.24 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-05 | 221 | 3 | 3 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| Kea behind a relay | `kea-relay` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-05 | 163 | 10 | 3 | 164 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| Kea failover pair | `kea-ha` | Debian 13, Docker 29.9.2 | v2.7.0-rc2 | v0.4.0 (67c28eb) | 2026-10-30 | 110 | 102 | 19 | 104 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc2.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc2-evidence.tar.gz) |
| ISC dhcpd | `isc-dhcp` | Debian 13, Docker 29.9.2 | v2.7.0-rc1 | v0.4.0 (b9b179a) | 2026-10-30 | 204 | 14 | 4 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc1-evidence.tar.gz) |
| ISC dhcpd on an Ubuntu host | `host-ubuntu2404-isc-dhcp` | Ubuntu 24.04, Docker 29.9.2 | v2.7.0-rc1 | v0.4.0 (b9b179a) | 2026-10-30 | 190 | 21 | 12 | 112 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc1-evidence.tar.gz) |
| ISC dhcpd failover pair | `isc-failover` | Debian 13, Docker 29.9.2 | v2.7.0-rc1 | v0.4.0 (b9b179a) | 2026-10-30 | 204 | 14 | 8 | 109 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc1-evidence.tar.gz) |
| dnsmasq | `dnsmasq` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-06 | 221 | 3 | 3 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| dnsmasq on an Ubuntu host | `host-ubuntu2404-dnsmasq` | Ubuntu 24.04, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-06 | 221 | 2 | 4 | 113 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| udhcpd (BusyBox) | `udhcpd` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (fc5c177) | 2026-11-05 | 111 | 0 | 0 | 229 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-evidence.tar.gz) |
| Pi-hole | `pihole` | Debian 13, Docker 29.9.2 | v2.7.0-rc2 | v0.4.0 (67c28eb) | 2026-10-30 | 127 | 3 | 8 | 197 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-rc2.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-rc2-evidence.tar.gz) |
| OpenWrt | `openwrt` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (9e83028) | 2026-11-06 | 155 | 0 | 0 | 185 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1-cell-reruns.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-cell-reruns-evidence.tar.gz) |
| Windows Server 2025 DHCP | `windows` | Debian 13, Docker 29.9.3 | v2.8.0-rc1 | v0.5.0 (aeb0016) | 2026-11-06 | 142 | 2 | 8 | 188 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/v0.5.0-plugin-v2.8.0-rc1-cell-reruns.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.5.0/lab-v0.5.0-plugin-v2.8.0-rc1-cell-reruns-evidence.tar.gz) |
| Home Fritz.Box, macvlan and macvlan-ipam only | `fritzbox-home` | Debian 13, Docker 26.1.5 | v2.7.0 | v0.4.0 (2c3e925) | 2026-10-31 | 31 | 3 | 0 | 102 | [page](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.4.0/results/v0.4.0-plugin-v2.7.0-fritzbox-home.md), [evidence](https://github.com/claymore666/docker-net-dhcp-lab/releases/download/v0.4.0/lab-v0.4.0-plugin-v2.7.0-fritzbox-home-evidence.tar.gz) |

This page is written by hand from the lab's results pages and is updated at
each release ([release runbook](release-runbook.md), step 9a). The lab's
[results index](https://github.com/claymore666/docker-net-dhcp-lab/blob/v0.5.0/results/README.md)
lists every run, including older plugin versions.

## Reading the counts

- PASS: the check held, and the verdict links its evidence.
- FAIL: the check did not hold. A FAIL row is a finding about the plugin
  only when the results page says so. Each page lists its FAIL and BLOCKED
  rows with their cause where one is known, and many were defects of the
  lab itself, fixed since. The full v0.5.0 run page states that no row of
  its run points at a plugin defect; the page of the OpenWrt and Windows
  reruns explains its FAIL and BLOCKED rows one by one. Read the cause list
  on the page before reading a count as a plugin problem.
- BLOCKED: the lab could not reach a known starting state for the scenario,
  for example a Docker host that did not come back from a reboot in time.
  The verdict names the reason.
- N/A: the scenario does not apply to that shape or server, for example a
  fixed-MAC reboot on `ipvlan`, whose containers share the parent's MAC.
  The verdict names the reason.

## Not tested

- No verdicts yet: the MikroTik RouterOS CHR cell (`chr`) has written no
  verdicts in any run. Its base image did not build in the v2.6.0-rc2 run,
  and its guest did not answer the lab in the v2.7.0-rc1 run.
- The Fritz.Box run is one home router with 18 scenarios that do not disturb
  the network: six fixed MAC addresses, no router login, no router setting
  changed, no IPv6, nothing that restarts the Docker host or its engine.
  Every other row of that page is N/A.
- Anything not in the table: other DHCP servers and routers, other router
  models and firmware, Docker hosts other than Debian 11, Debian 13 and
  Ubuntu 24.04, and engine versions other than the ones listed.
- A cell is not re-run for every plugin version, so a row can be behind
  the newest plugin release.
