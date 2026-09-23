// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import "io"

// Docker destroys the plugin rootfs holding -logfile on every `plugin rm` and `install`, the supported upgrade path;
// a v1.4.0 production upgrade lost the outgoing plugin's log that way.
// dockerd captures a managed plugin's stdout into the daemon log on the host, which survives. The file stays because
// the integration suite's whole-run fault census reads it (#385).

// pluginLogWriter fans the plugin's log to both sinks (#420).
func pluginLogWriter(stdout, file io.Writer) io.Writer {
	if file == nil {
		return stdout
	}
	if stdout == nil {
		return file
	}
	return io.MultiWriter(stdout, file)
}
