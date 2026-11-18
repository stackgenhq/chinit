// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/stackgenhq/chinit/pkg/chinit/app"
)

// chinit is the cloud-adaptive ClickHouse node bootstrapper and
// lifecycle manager for cloud VMs.
func main() {
	app.Main(os.Args[1:])
}
