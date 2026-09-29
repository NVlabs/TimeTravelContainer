/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

use ttcx::Client;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = std::env::args()
        .nth(1)
        .ok_or("usage: status ROOT, the TTC_ROOT (or TTC_LOCAL_ROOT) of the ttcd to use")?;
    let status = Client::new(root).ping()?;
    println!("{}", serde_json::to_string_pretty(&status)?);
    Ok(())
}
