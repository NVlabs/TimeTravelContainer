// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

/*
// ttcd's Info.plist, in the binary's section that macOS reads it of, as
// CFBundleGetMainBundle does.
__attribute__((used, section("__TEXT,__info_plist")))
static const char info_plist[] =
	"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
	"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"
	"<plist version=\"1.0\">\n"
	"<dict>\n"
	"\t<key>CFBundleIdentifier</key>\n"
	"\t<string>com.nvidia.ttcd</string>\n"
	"\t<key>CFBundleName</key>\n"
	"\t<string>ttcd</string>\n"
	"</dict>\n"
	"</plist>\n";
*/
import "C"
