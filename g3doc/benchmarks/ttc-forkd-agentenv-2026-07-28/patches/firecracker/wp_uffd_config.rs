// Reconstructed for benchmarking from deeplethe/firecracker commit
// 7d80afade831f01e3178c5b76daf8b4cbc02d176, whose commit message and call
// sites reference this file but whose Git tree omits it.

use std::path::PathBuf;

use serde::Deserialize;

/// Parameters for arming write-protect userfaultfd handling on guest memory.
#[derive(Debug, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SetupWpUffdParams {
    /// Unix socket on which the caller is waiting to receive the userfaultfd.
    pub socket: PathBuf,
}
