package main

import "io"

// pluginLogWriter fans the plugin's log to both sinks (#420).
//
// file is the -logfile inside the plugin rootfs. Docker destroys that
// rootfs on every `plugin rm` / `install`, which is the supported
// upgrade path — so on its own it guarantees that the previous
// version's history is gone at exactly the moment an operator wants it.
//
// stdout of a managed plugin is captured by dockerd and lands in the
// daemon log on the HOST filesystem, which survives the plugin being
// removed.
//
// Both, not either. Dropping the file is the obvious-looking fix and
// breaks the integration suite's whole-run fault census, which reads it
// (#385). Dropping stdout is the status quo that loses production
// history on every upgrade.
func pluginLogWriter(stdout, file io.Writer) io.Writer {
	if file == nil {
		return stdout
	}
	if stdout == nil {
		return file
	}
	return io.MultiWriter(stdout, file)
}
